package gitservice

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/itoworld/yor/src/common/logger"
	"github.com/itoworld/yor/src/common/structure"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

type GitService struct {
	gitRootDir          string
	repoRootDir         string // absolute path to the git repository root (where .git lives)
	scanPathFromRoot    string
	repository          *git.Repository
	remoteURL           string
	organization        string
	repoName            string
	BlameByFile         *sync.Map
	PreviousBlameByFile *sync.Map
	currentUserEmail    string
	previousCommit      *object.Commit // cached during init to avoid concurrent go-git access
}

func NewGitService(rootDir string) (*GitService, error) {
	var repository *git.Repository
	var err error
	rootDirIter, _ := filepath.Abs(rootDir)
	for {
		repository, err = git.PlainOpen(rootDirIter)
		if err == nil {
			break
		}
		newRootDir := filepath.Dir(rootDirIter)
		if rootDirIter == newRootDir {
			break
		}
		rootDirIter = newRootDir
	}
	if err != nil {
		return nil, err
	}

	scanAbsDir, _ := filepath.Abs(rootDir)
	scanPathFromRoot, _ := filepath.Rel(rootDirIter, scanAbsDir)

	gitService := GitService{
		gitRootDir:          rootDir,
		repoRootDir:         rootDirIter,
		scanPathFromRoot:    scanPathFromRoot,
		repository:          repository,
		BlameByFile:         &sync.Map{},
		PreviousBlameByFile: &sync.Map{},
	}
	err = gitService.setOrgAndName()
	gitService.currentUserEmail = GetGitUserEmail()

	// Cache the previous commit during init to avoid concurrent access to
	// the go-git Repository object later. go-git's Repository is not
	// thread-safe, but GetPreviousBlameResult was previously calling
	// repository.Head()/CommitObject()/Parents() from multiple goroutines.
	if repository != nil {
		ref, refErr := repository.Head()
		if refErr == nil {
			commit, commitErr := repository.CommitObject(ref.Hash())
			if commitErr == nil {
				parentIter := commit.Parents()
				previousCommit, parentErr := parentIter.Next()
				if parentErr == nil {
					gitService.previousCommit = previousCommit
				}
			}
		}
	}

	return &gitService, err
}

func (g *GitService) setOrgAndName() error {
	// get remotes to find the repository's url
	remotes, err := g.repository.Remotes()
	if err != nil {
		return fmt.Errorf("failed to get remotes, err: %s", err)
	}

	for _, remote := range remotes {
		if remote.Config().Name == "origin" {
			g.remoteURL = remote.Config().URLs[0]
			// get endpoint structured like '/github.com/itoworld/yor.git
			endpoint, err := transport.NewEndpoint(g.remoteURL)
			if err != nil {
				return err
			}
			// remove leading '/' from path and trailing '.git. suffix, then split by '/'
			endpointPathParts := strings.Split(strings.TrimSuffix(strings.TrimLeft(endpoint.Path, "/"), ".git"), "/")
			if len(endpointPathParts) < 2 {
				return fmt.Errorf("invalid format of endpoint path: %s", endpoint.Path)
			}
			g.organization = endpointPathParts[0]
			g.repoName = strings.Join(endpointPathParts[1:], "/")
			break
		}
	}

	return nil
}

func (g *GitService) ComputeRelativeFilePath(fp string) string {
	if strings.HasPrefix(fp, g.gitRootDir) {
		res, _ := filepath.Rel(g.gitRootDir, fp)
		return filepath.Join(g.scanPathFromRoot, res)
	}
	scanPathIter := g.scanPathFromRoot
	parent := filepath.Dir(fp)
	for {
		_, child := filepath.Split(scanPathIter)
		if parent != child {
			break
		}
		scanPathIter, _ = filepath.Split(scanPathIter)
	}
	return filepath.Join(scanPathIter, fp)
}

func (g *GitService) GetBlameForFileLines(filePath string, lines structure.Lines) (*GitBlame, error) {
	logger.Info(fmt.Sprintf("Getting git blame for %v (%v:%v)", filePath, lines.Start, lines.End))
	relativeFilePath := g.ComputeRelativeFilePath(filePath)
	blame, ok := g.BlameByFile.Load(filePath)
	if ok {
		return NewGitBlame(relativeFilePath, filePath, lines, blame.(*git.BlameResult), g), nil
	}

	var err error
	blame, err = g.GetFileBlame(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to get blame for latest commit of file %s because of error %s", filePath, err)
	}

	g.BlameByFile.Store(filePath, blame)

	return NewGitBlame(relativeFilePath, filePath, lines, blame.(*git.BlameResult), g), nil
}

func (g *GitService) GetOrganization() string {
	return g.organization
}

func (g *GitService) GetRepoName() string {
	return g.repoName
}

func (g *GitService) GetFileBlame(filePath string) (*git.BlameResult, error) {
	blame, ok := g.BlameByFile.Load(filePath)
	if ok {
		return blame.(*git.BlameResult), nil
	}

	relativeFilePath := g.ComputeRelativeFilePath(filePath)

	// Use native git blame instead of go-git's slow in-process implementation.
	// Native git is 10-100x faster and each invocation is a separate process,
	// so no global lock is needed for concurrent access.
	blameResult, err := nativeGitBlame(g.repoRootDir, relativeFilePath, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("failed to get blame for file %s: %s", filePath, err)
	}

	// Get blame for previous commit (HEAD~1) for change detection
	previousBlame, err := nativeGitBlame(g.repoRootDir, relativeFilePath, "HEAD~1")
	if err != nil {
		// Previous blame may fail for new files or repos with a single commit - this is non-fatal
		logger.Info(fmt.Sprintf("Could not get previous blame for %s: %s", filePath, err))
	}

	g.BlameByFile.Store(filePath, blameResult)
	if previousBlame != nil {
		g.PreviousBlameByFile.Store(filePath, previousBlame)
	}

	return blameResult, nil
}

func GetGitUserEmail() string {
	log.SetOutput(io.Discard)
	cmd := exec.Command("git", "config", "user.email")
	email, err := cmd.Output()
	stdout := os.Stdout
	log.SetOutput(stdout)
	if err != nil {
		logger.Debug(fmt.Sprintf("unable to get current git user email: %s", err))
		return ""
	}
	return strings.ReplaceAll(string(email), "\n", "")
}
