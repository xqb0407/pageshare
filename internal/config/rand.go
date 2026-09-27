package config

import "crypto/rand"

func cryptorandRead(b []byte) (int, error) { return rand.Read(b) }
