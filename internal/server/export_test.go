package server

// InFlight reports how many downloads the server's store is still running,
// so pure-s3 characterization can wait for an upload before asserting a hit.
func InFlight(s *Server) int { return len(s.store.InFlight()) }
