package gen

import (
	"embed"

	"github.com/hostryhq/go-auth"
	gsync "github.com/hostryhq/go-common/sync"
)

//go:embed catalogue.json
var catalogueJSON []byte

//go:embed entities
var entitiesFS embed.FS

// Catalogue returns the permission catalogue generated from api-specs/common/permissions.yaml.
func Catalogue() (*auth.Catalogue, error) { return auth.LoadCatalogue(catalogueJSON) }

// Registry returns the synced-entity registry (api-specs entities/, copied by make gen) that the projection engine
// applies: owner, policy and field rules per entity.
func Registry() (*gsync.Registry, error) { return gsync.LoadRegistry(entitiesFS, "entities") }
