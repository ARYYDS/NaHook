#!/usr/bin/env bash
# T0 client: four combinations x 3 runs. Each curl disconnects after 2s.
# Writes one JSON line per run to /root/nahook-t0/client.log.
# Runtime is about 12 x 27s = 5.4 minutes; start it with nohup.
set -u
OUT=/root/nahook-t0/client.log
for path in hold-neg1 hold-default stream-neg1 stream-default; do
  for i in 1 2 3; do
    start=$(date +%s.%3N)
    curl -sS -N -o /dev/null -m 2 -X POST -H 'Content-Type: application/json' \
      -d '{"stream":true}' "http://127.0.0.1:19444/${path}/t${i}"
    rc=$?
    end=$(date +%s.%3N)
    printf '{"path":"%s","i":%d,"curl_start":%s,"curl_end":%s,"rc":%d}\n' \
      "$path" "$i" "$start" "$end" "$rc" >> "$OUT"
    sleep 25
  done
done
echo '{"done":true}' >> "$OUT"
