// Package gateway is wxingress's thin layer over the Gateway API graph of
// NGINX Gateway Fabric, which hack/sync-ngf.sh copies unmodified into
// internal/third_party/ngf. It fills the graph's input from client-go
// listers, maps its result onto VirtualServers that take the same path as
// VirtualServers from the API, and writes the Gateway API status back.
package gateway
