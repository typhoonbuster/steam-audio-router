package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// runService is the main loop: supervise pw-dump, react to its events, poll
// the Steam log, verify pending routes, and stop cleanly on SIGTERM/SIGINT.
// In `once` mode it processes the initial dump, prints a report and exits.
func runService(r *Router, once bool) int {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigs)

	backoff := r.cfg.PwRestartBackoff
	fails := 0
	ticker := time.NewTicker(secs(r.cfg.PollInterval))
	defer ticker.Stop()

	sleep := func(d time.Duration) bool { // false when interrupted by a signal
		select {
		case <-sigs:
			return false
		case <-time.After(d):
			return true
		}
	}
	nextBackoff := func() time.Duration {
		i := fails
		if i >= len(backoff) {
			i = len(backoff) - 1
		}
		fails++
		return secs(backoff[i])
	}

	for {
		dump, err := startPwDump(!once)
		if err != nil {
			Errorf("Cannot start pw-dump: %v (is pipewire installed?)", err)
			if once {
				return 2
			}
			if !sleep(nextBackoff()) {
				r.Shutdown()
				return 0
			}
			continue
		}
		r.ResetState()
		if once {
			Infof("Connected to PipeWire via pw-dump")
		} else {
			Infof("Connected to PipeWire via pw-dump -m")
		}
		nextReconcile := time.Now()

	connected:
		for {
			select {
			case <-sigs:
				Infof("Signal received, shutting down")
				r.Shutdown()
				dump.Stop()
				Infof("Clean shutdown")
				return 0

			case batch, ok := <-dump.Batches:
				if !ok {
					err := dump.Wait()
					if once {
						r.tailer.Poll()
						r.Reconcile("once")
						fmt.Print(r.Report())
						return 0
					}
					if err != nil {
						Warnf("pw-dump exited (PipeWire restarted or stopped?): %v", err)
					} else {
						Warnf("pw-dump exited (PipeWire restarted or stopped?)")
					}
					if !sleep(nextBackoff()) {
						r.Shutdown()
						return 0
					}
					break connected
				}
				fails = 0
				if r.HandleBatch(batch) && !once {
					r.Reconcile("event")
				}

			case <-ticker.C:
				if once {
					continue
				}
				if r.tailer.Poll() {
					r.Reconcile("steam log")
				}
				r.CheckPending()
				if now := time.Now(); !now.Before(nextReconcile) {
					nextReconcile = now.Add(secs(r.cfg.ReconcileInterval))
					r.Reconcile("periodic")
				}
			}
		}
	}
}
