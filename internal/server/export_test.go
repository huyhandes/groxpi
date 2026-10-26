package server

// InFlight reports how many downloads the server's store is still running,
// so pure-s3 characterization can wait for an upload before asserting a hit.
func InFlight(s *Server) int { return len(s.store.InFlight()) }

// InFlightRequests reports how many requests share the server's running
// downloads, so characterization can tell when a follower has joined a flight.
func InFlightRequests(s *Server) int {
	n := 0
	for _, p := range s.store.InFlight() {
		n += p.Requests
	}
	return n
}
