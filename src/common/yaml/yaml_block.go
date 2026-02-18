package yaml

import "github.com/itoworld/yor/src/common/structure"

type IYamlBlock interface {
	structure.IBlock
	UpdateTags()
	GetFramework() string
}
