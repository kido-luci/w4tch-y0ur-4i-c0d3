// Package fdgauge periodically logs how many file descriptors the process
// holds, broken down by kind. Fd exhaustion presents as unrelated failures —
// ReadDir errors, accept errors — long after the leak started, and restarting
// the process destroys the evidence; a census in the log is what makes the
// growth curve, and the kind of descriptor doing the growing, reconstructable
// afterwards.
package fdgauge

import (
	"log"
	"time"
)

// Every logs a census now and then on each tick, forever. On platforms
// without a census (windows) it does nothing.
func Every(interval time.Duration) {
	if !supported {
		return
	}
	logCensus()
	go func() {
		for range time.Tick(interval) {
			logCensus()
		}
	}()
}

func logCensus() {
	c, err := census()
	if err != nil {
		// Failing to read our own fd table is itself the signal: under full
		// exhaustion the gauge reports the error instead of a count.
		log.Printf("fdgauge: %v", err)
		return
	}
	log.Printf("fdgauge: %s", c)
}
