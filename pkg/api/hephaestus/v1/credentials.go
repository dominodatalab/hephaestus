package v1

type BasicAuthCredentials struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

type SecretCredentials struct {
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

type RegistryCredentials struct {
	Server string `json:"server,omitempty"`

	CloudProvided *bool                 `json:"cloudProvided,omitempty"`
	BasicAuth     *BasicAuthCredentials `json:"basicAuth,omitempty"`
	Secret        *SecretCredentials    `json:"secret,omitempty"`
}
