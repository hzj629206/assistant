package agent

import "sync"

type keyedLocker struct {
	mu      sync.Mutex
	entries map[string]*lockEntry
}

type lockEntry struct {
	refs int
	mu   sync.Mutex
}

func newKeyedLocker() *keyedLocker {
	return &keyedLocker{
		entries: make(map[string]*lockEntry),
	}
}

func (l *keyedLocker) Lock(key string) func() {
	l.mu.Lock()
	entry, ok := l.entries[key]
	if !ok {
		entry = &lockEntry{}
		l.entries[key] = entry
	}
	entry.refs++
	l.mu.Unlock()

	entry.mu.Lock()

	return func() {
		entry.mu.Unlock()

		l.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(l.entries, key)
		}
		l.mu.Unlock()
	}
}
