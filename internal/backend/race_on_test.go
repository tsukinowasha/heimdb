//go:build race

package backend

// raceEnabled は、-race でテストを流しているか。データ競合の検出はコードを数倍遅くするので、時間で確かめるテストは
// 上限をゆるめる（CI の -race で TestStage111LargeInList の 2 秒を超えた。段階 114）。
const raceEnabled = true
