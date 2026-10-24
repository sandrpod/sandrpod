package egress

import "crypto/x509"

// poolSubjects extracts the certificates from a CertPool. The stdlib does not
// expose them, so the pool is cloned and its subjects enumerated: Clone is the
// documented way to read a pool without mutating it.
func poolSubjects(pool *x509.CertPool) [][]byte {
	subjects := pool.Subjects() //nolint:staticcheck // the only accessor the stdlib provides
	return subjects
}
