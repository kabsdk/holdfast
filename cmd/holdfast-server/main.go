// holdfast-server: the configurator and the maintenance leader.
//
//	holdfast-server -config /etc/holdfast/server.yaml
//	holdfast-server keygen /etc/holdfast/credentials.key
package main

import (
	"github.com/kabsdk/holdfast/internal/server"
	"github.com/kabsdk/holdfast/internal/serverd"
)

func main() { serverd.Main("holdfast-server", "/etc/holdfast/server.yaml", server.RoleServer) }
