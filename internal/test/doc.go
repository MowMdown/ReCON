// Package test has everything ReCON's tests share: a fake BattlEye RCON
// server (Server), a throwaway player database (OpenDB) with helpers to
// read it back, and two valid GUIDs. Only _test.go files import it, so none
// of it is built into the program.
//
// It may only import low-level packages (battleye, store): a package's own
// tests can't import a package that imports it back.
package test
