package seatalk

// Config contains the SeaTalk-specific credentials and callback settings.
type Config struct {
	AppID               string `json:"app_id" yaml:"app_id"`
	AppSecret           string `json:"app_secret" yaml:"app_secret"`
	SigningSecret       string `json:"signing_secret" yaml:"signing_secret"`
	EmployeeInfoEnabled bool   `json:"employee_info_enabled" yaml:"employee_info_enabled"`
}
