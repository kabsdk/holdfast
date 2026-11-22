// holdfast-server: the configurator, the gateway, the maintenance leader, and with offsite: in its settings the
// off-site copies too (decision 041).
//
//	holdfast-server -config /etc/holdfast/server.yaml
//	holdfast-server keygen /etc/holdfast/credentials.key
package main

import "github.com/kabsdk/holdfast/internal/serverd"

func main() { serverd.Main("holdfast-server", "/etc/holdfast/server.yaml") }
