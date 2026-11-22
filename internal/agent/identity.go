package agent

// identityFile holds the agent's own certificate and then its private key, as PEM, root-only (pin.NewIdentity). The
// server pinned the certificate at enrollment (decision 041), and restic reads the same file for the gateway.
const identityFile = "agent.pem"
