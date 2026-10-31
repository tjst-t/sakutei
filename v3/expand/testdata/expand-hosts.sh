#!/bin/sh
# 外のコマンドの Expand の例（テスト用）：intent の hosts: の名前ごとに dns/record を作る。
# 入力の JSON から名前を拾うだけの決定的な変換（同じ入力に同じ出力）。
names=$(tr -d '\n ' | sed -n 's/.*"hosts":\[{"doc":"[^"]*","line":[0-9]*,"value":\[\([^]]*\)\]}\].*/\1/p' | tr -d '"' | tr ',' ' ')
printf '{"resources": ['
sep=""
for n in $names; do
  printf '%s{"type": "dns/record", "id": "%s", "fields": {"value": "192.0.2.1"}, "reason": "exec"}' "$sep" "$n"
  sep=", "
done
printf '], "diagnostics": [{"severity": "warn", "target": "hosts", "message": "%s 台"}]}\n' "$(echo $names | wc -w | tr -d ' ')"
