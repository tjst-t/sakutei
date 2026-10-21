// Copyright 2026 tjst-t
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package acceptance

import (
	"bytes"
	"sync"
)

// syncBuffer is a bytes.Buffer guarded by a mutex.
//
// Daemon helpers (startDaemon / startDaemonCombined / startDaemonWithArgs)
// hand the buffer to exec.Cmd as Stdout/Stderr. os/exec copies the child's
// output into it from its own goroutine while the test goroutine polls it
// (waitForStringInBuf / String()). A bare bytes.Buffer is not safe for that
// concurrent use and trips the race detector, so every access is locked.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *syncBuffer) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Len()
}

func (s *syncBuffer) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf.Reset()
}
