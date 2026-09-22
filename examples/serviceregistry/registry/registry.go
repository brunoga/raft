// Package registry holds what the registry server and the agents registering
// with it both have to agree on.
//
// It is a package rather than a copy in each program on purpose: a registry
// and the things registering with it disagreeing about the shape of an entry,
// or about the key it goes under, is a bug that presents as an empty
// registry rather than as an error anywhere.
package registry

// CollectionName is the easyraft collection instances are registered in.
const CollectionName = "services"

// Instance is one registered instance of a service.
type Instance struct {
	Service string            `json:"service"`
	ID      string            `json:"id"`
	Addr    string            `json:"addr"`
	Meta    map[string]string `json:"meta,omitempty"`
}

// Key returns the key an instance is registered under.
//
// The service name comes first so that every instance of one service shares a
// prefix, which is what makes "who is running this service" a single prefix
// scan rather than a filter over the whole registry.
func Key(service, id string) string {
	return service + "/" + id
}

// Prefix returns the key prefix covering every instance of one service.
func Prefix(service string) string {
	return service + "/"
}
