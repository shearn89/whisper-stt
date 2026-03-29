//go:build !noX11

package main

import (
	"sync"

	hook "github.com/robotn/gohook"
)

// runHoldListener starts recording when all combo keys are held and stops
// (triggering transcription) when any of them is released.
func runHoldListener(rec *recorder, cfg *config, keys []string) {
	hook.Register(hook.KeyDown, keys, func(e hook.Event) {
		if !rec.isActive() {
			rec.start()
		}
	})

	// stopAndGet is atomic so only the first goroutine that wins does work.
	for _, k := range keys {
		k := k
		hook.Register(hook.KeyUp, []string{k}, func(e hook.Event) {
			go func() {
				samples, ok := rec.stopAndGet()
				if !ok {
					return
				}
				handleSamples(samples, cfg)
			}()
		})
	}

	s := hook.Start()
	defer hook.End()
	<-hook.Process(s)
}

// runToggleListener toggles recording on each successive press of the combo.
func runToggleListener(rec *recorder, cfg *config, keys []string) {
	var mu sync.Mutex
	toggled := false

	hook.Register(hook.KeyDown, keys, func(e hook.Event) {
		mu.Lock()
		defer mu.Unlock()
		if !toggled {
			toggled = true
			rec.start()
		} else {
			toggled = false
			go func() {
				samples, ok := rec.stopAndGet()
				if !ok {
					return
				}
				handleSamples(samples, cfg)
			}()
		}
	})

	s := hook.Start()
	defer hook.End()
	<-hook.Process(s)
}
