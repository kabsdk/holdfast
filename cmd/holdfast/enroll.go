package main

import (
	"fmt"
	"time"
)

// enrollment is the API's answer wherever a host needs enrolling: host add, enroll-code, rotate and import.
type enrollment struct {
	Code           string    `json:"code"`
	ExpiresAt      time.Time `json:"expires_at"`
	InstallScript  string    `json:"install_script"`
	InstallCommand string    `json:"install_command"`
}

// howTo is what to run on the host: the one-line install (decision 044), or the enrollment alone where the agent's
// packages are installed already, such as by configuration management.
func (e enrollment) howTo() string {
	return fmt.Sprintf("enrollment code %s, valid until %s (one use)\n\nrun on the host, as root:\n  %s\n\nor, where the agent's packages are installed already:\n  %s\n",
		e.Code, e.ExpiresAt.Local().Format("15:04"), e.InstallScript, e.InstallCommand)
}
