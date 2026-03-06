package types

// TeamManifest represents the parsed team manifest (teams/TEAM_ID.yaml).
type TeamManifest struct {
	APIVersion string       `yaml:"apiVersion" json:"apiVersion"`
	Kind       string       `yaml:"kind" json:"kind"`
	Metadata   TeamMetadata `yaml:"metadata" json:"metadata"`
	Spec       TeamSpec     `yaml:"spec" json:"spec"`
}

type TeamMetadata struct {
	ID     string            `yaml:"id" json:"id"`
	Labels map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
}

type TeamSpec struct {
	Lead          string            `yaml:"lead,omitempty" json:"lead,omitempty"`
	Resources     TeamResources     `yaml:"resources,omitempty" json:"resources,omitempty"`
	Communication TeamCommunication `yaml:"communication,omitempty" json:"communication,omitempty"`
	SharedVolumes []SharedVolume    `yaml:"shared_volumes,omitempty" json:"shared_volumes,omitempty"`
}

type TeamResources struct {
	MaxMemory string `yaml:"maxMemory,omitempty" json:"maxMemory,omitempty"`
	MaxAgents int    `yaml:"maxAgents,omitempty" json:"maxAgents,omitempty"`
}

type TeamCommunication struct {
	Namespace             string      `yaml:"namespace,omitempty" json:"namespace,omitempty"`
	Persistent            bool        `yaml:"persistent,omitempty" json:"persistent,omitempty"`
	HistoryDepth          int         `yaml:"historyDepth,omitempty" json:"historyDepth,omitempty"`
	CrossTeamCapabilities interface{} `yaml:"crossTeamCapabilities,omitempty" json:"crossTeamCapabilities,omitempty"`
	AllowedCallers        []string    `yaml:"allowedCallers,omitempty" json:"allowedCallers,omitempty"`
}

type SharedVolume struct {
	Name     string `yaml:"name" json:"name"`
	HostPath string `yaml:"hostPath" json:"hostPath"`
	Access   string `yaml:"access,omitempty" json:"access,omitempty"`
}
