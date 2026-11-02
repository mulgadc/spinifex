package dispatch

import "net/http"

// PreAuth serves a request that carries no credentials ahead of
// authentication and throttling, because its own payload authenticates it.
// Serve reports handled=false, having written nothing and left the body
// readable, for a request that is not its own. An error it returns for a
// handled request renders under Service.
type PreAuth struct {
	Service string
	Serve   func(w http.ResponseWriter, r *http.Request) (handled bool, err error)
}
