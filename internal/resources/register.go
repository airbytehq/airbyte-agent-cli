package resources

import "github.com/airbytehq/airbyte-agent-cli/internal/registry"

func RegisterAll() {
	registry.Register(&organizationsResource{})
	registry.Register(&workspacesResource{})
	registry.Register(&connectorsResource{})
	registry.Register(&skillsResource{})
}
