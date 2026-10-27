# Copyright 2026 tjst-t
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Sakutei の ansible provider（internal/ansible）が ad-hoc の ansible に読ませる
# stdout callback。ansible-core は JSON の stdout callback を持たない（json は
# ansible.posix にある）ので、ホストの結果ごとに 1 行の JSON を出すだけのものを
# 自前で持つ。行は "SAKUTEI-RESULT " で始まる（ほかの callback の出力と混ざらない）。
# 出すのは host・status（ok / failed / unreachable / skipped）・changed・msg・
# diff（--diff の差分を ansible 自身の形にしたもの）だけ。
from __future__ import annotations

DOCUMENTATION = """
    name: sakutei_json
    type: stdout
    short_description: one JSON line per host result, for Sakutei
    description:
        - Used by the Sakutei ansible provider. Prints one line per host result.
"""

import json
import sys

from ansible.plugins.callback import CallbackBase


class CallbackModule(CallbackBase):
    CALLBACK_VERSION = 2.0
    CALLBACK_TYPE = 'stdout'
    CALLBACK_NAME = 'sakutei_json'

    def _emit(self, status, result):
        res = getattr(result, 'result', None)
        if res is None:
            res = result._result
        host = getattr(result, 'host', None) or result._host
        msg = res.get('msg') or res.get('module_stderr') or res.get('stderr') or ''
        out = {
            'host': host.get_name(),
            'status': status,
            'changed': bool(res.get('changed', False)),
            'msg': msg if isinstance(msg, str) else json.dumps(msg, default=str),
            'diff': '',
        }
        if res.get('diff'):
            try:
                out['diff'] = self._get_diff(res['diff'])
            except Exception:
                out['diff'] = ''
        sys.stdout.write('SAKUTEI-RESULT ' + json.dumps(out, default=str, sort_keys=True) + '\n')
        sys.stdout.flush()

    def v2_runner_on_ok(self, result):
        self._emit('ok', result)

    def v2_runner_on_failed(self, result, ignore_errors=False):
        self._emit('failed', result)

    def v2_runner_on_unreachable(self, result):
        self._emit('unreachable', result)

    def v2_runner_on_skipped(self, result):
        self._emit('skipped', result)
