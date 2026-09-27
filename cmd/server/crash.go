package main

/*
static void native_crash(void) { volatile int *p = 0; *p = 1; }
*/
import "C"

// nativeCrash simulates a native/cgo fault (e.g. inside ONNX Runtime). Go's
// recover() cannot contain it: the whole process dies.
func nativeCrash() { C.native_crash() }
