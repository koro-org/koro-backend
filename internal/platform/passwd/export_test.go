package passwd

// HashWithParams exposes hashing with arbitrary (old) parameters to tests.
func HashWithParams(h *Hasher, pw string, memory, time uint32, threads uint8) (string, error) {
	return h.hashWith(pw, memory, time, threads)
}
