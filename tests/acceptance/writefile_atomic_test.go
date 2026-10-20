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
	"os"
	"path/filepath"
)

// writeFileAtomic is a drop-in replacement for os.WriteFile used by every
// acceptance test that writes a file the sakutei daemon may be reading at the
// same time (state files for drift injection, intent files, zone/ruleset files).
//
// os.WriteFile truncates the target first and then writes, so a concurrently
// running daemon could observe an empty or half-written file and treat it as
// unparseable (AvailabilityUnknown) or as a loader error — a timing-dependent
// flake. Here the data goes to a temp file in the same directory and is then
// os.Rename'd over the target, so readers only ever see the old or the new
// complete content.
//
// The temp name starts with "." and has no .json/.yaml/.yml suffix, so the
// intent loader (which only reads those suffixes) never picks it up.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}
