//go:build race

package channel

// raceEnabled lets tests shrink CPU-heavy fixtures under the race detector.
const raceEnabled = true
