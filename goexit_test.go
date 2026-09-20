package testdb

import "runtime"

// runtimeGoexit mimics testing.TB.Skipf, which never returns.
func runtimeGoexit() { runtime.Goexit() }
