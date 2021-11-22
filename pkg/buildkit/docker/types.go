package docker

type AuthConfig struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type AuthConfigs map[string]AuthConfig

type Config struct {
	Auths AuthConfigs `json:"auths"`
}
