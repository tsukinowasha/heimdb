//go:build !race

package backend

// raceEnabled は、-race でテストを流しているか（race_on_test.go）。
const raceEnabled = false
