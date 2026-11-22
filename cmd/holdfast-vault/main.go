// holdfast-vault: off-site copies of another site's hosts (decisions 027 and 039). It is holdfast-server in the vault
// role, with its own settings, database and bucket in its own site.
//
//	holdfast-vault -config /etc/holdfast-vault/vault.yaml
//	holdfast-vault keygen /etc/holdfast-vault/credentials.key
package main

import (
	"github.com/kabsdk/holdfast/internal/server"
	"github.com/kabsdk/holdfast/internal/serverd"
)

func main() { serverd.Main("holdfast-vault", "/etc/holdfast-vault/vault.yaml", server.RoleVault) }
