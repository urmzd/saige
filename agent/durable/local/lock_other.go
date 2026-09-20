//go:build !unix

package local

import "errors"

func lock(string) (func(), error) { return nil, errors.New("local durable worker locks require Unix") }
