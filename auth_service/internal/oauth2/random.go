package oauth2

import "crypto/rand"

func readRandom(b []byte) (int, error) { return rand.Read(b) }
