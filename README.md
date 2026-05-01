# speedtest

Loops `scp` downloads from server B to server A, measuring real wall-clock
download speed for each iteration. Writes a human log + a JSON file with the
full array of `{startTime, endTime, averageDownloadSpeedMBps, ...}` objects,
plus summary stats including how many iterations were slower than your
configured threshold.

## Build (on server A, where you'll run it)

```bash
go mod tidy        # downloads gopkg.in/yaml.v3
go build -o speedtest .
```

That produces a single static binary. No runtime dependencies.

## One-time setup on server B

Put a test file on B that A can pull. A few hundred MB is a good size — big
enough to actually saturate the link, small enough that 24 h of loops doesn't
melt the disk.

```bash
# on server B
dd if=/dev/urandom of=/srv/speedtest.bin bs=1M count=200
```

Make sure passwordless ssh from A → B works (key-based auth). The tool runs
scp in `BatchMode=yes`, so it will fail fast instead of hanging on a password
prompt.

```bash
# on server A
ssh-copy-id user@192.168.1.50    # if you haven't already
ssh user@192.168.1.50 'echo ok'  # must print "ok" with no prompt
```

## Configuration

You can configure the tool two ways — pick whichever feels nicer:

### Option 1: config.yaml (recommended for long runs)

Edit `config.yaml`:

```yaml
source: "root@192.168.1.50:/srv/speedtest.bin"
duration: "24h"
port: 22
ssh_key: ""
logs_dir: "logs"
interval: "0s"
slow_threshold_mbps: 5
```

Then just run:

```bash
./speedtest
```

If `config.yaml` exists in the current folder, it's loaded automatically — no
flag needed. To use a config from a different path, pass it explicitly:

```bash
./speedtest -config /etc/speedtest/prod.yaml
```

### Option 2: command-line flags

```bash
./speedtest \
  -source root@192.168.1.50:/srv/speedtest.bin \
  -duration 24h \
  -p 22 \
  -slow-threshold 5
```

### Option 3: both — flags override the config file

Useful when you mostly use the config but want to test a one-off setting:

```bash
./speedtest -duration 5m
```

(`config.yaml` is auto-loaded, but `-duration 5m` overrides whatever's in it.)

## All flags

| Flag | Config key | Default | Description |
|---|---|---|---|
| `-config` | — | auto: `./config.yaml` if present | path to YAML config file |
| `-source` | `source` | **required** | `user@ip:/path/to/file` |
| `-duration` | `duration` | `5m` | `30s`, `5m`, `1h`, `24h`, `1h30m` |
| `-p` | `port` | `22` | SSH port on server B |
| `-i` | `ssh_key` | `""` | SSH private key file |
| `-logs` | `logs_dir` | `logs` | folder for output |
| `-interval` | `interval` | `0s` | pause between iterations |
| `-scp-args` | `scp_args` | `""` | extra args for scp |
| `-slow-threshold` | `slow_threshold_mbps` | `5` | iterations below this MB/s count as "slow" |

## Examples

```bash
# 5-minute smoke test with IP address (no config file used)
./speedtest -duration 5m -source root@192.168.1.50:/srv/speedtest.bin

# Full 24-hour run — uses ./config.yaml automatically
./speedtest

# Use a config from elsewhere
./speedtest -config /etc/speedtest/prod.yaml

# Custom port + ssh key
./speedtest \
  -source ubuntu@10.0.0.20:/data/test.bin \
  -p 2222 \
  -i ~/.ssh/id_ed25519 \
  -duration 1h

# Override one setting from config.yaml without editing the file
./speedtest -interval 2s
./speedtest -slow-threshold 50
./speedtest -duration 30m
```

Press Ctrl+C to stop early — it finishes the current iteration and writes
the JSON cleanly.

## Output

In the `logs/` dir (or whatever `logs_dir` you set):

- `speedtest-YYYYMMDD-HHMMSS.log` — human-readable log, one line per iteration
- `speedtest-YYYYMMDD-HHMMSS.json` — full results, **rewritten after every
  iteration** so a crash / power loss never loses data

JSON shape:

```json
{
  "startedAt": "2026-05-01T13:45:00+04:00",
  "finishedAt": "2026-05-02T13:45:12+04:00",
  "plannedDuration": "24h",
  "actualDuration": "24h0m12s",
  "source": "root@192.168.1.50:/srv/speedtest.bin",
  "totalRuns": 1843,
  "successful": 1841,
  "failed": 2,
  "minMBps": 4.81,
  "maxMBps": 112.34,
  "avgMBps": 87.22,
  "slowThresholdMBps": 5,
  "slowRuns": 12,
  "slowRunsPercent": 0.65,
  "tests": [
    {
      "iteration": 1,
      "startTime": "2026-05-01T13:45:00+04:00",
      "endTime":   "2026-05-01T13:45:03+04:00",
      "durationSeconds": 2.98,
      "fileSizeBytes": 209715200,
      "averageDownloadSpeedMBps": 67.12,
      "averageDownloadSpeedMbps": 562.94,
      "success": true
    }
  ]
}
```

### Reading the slow-runs fields

- **`slowRuns`** — how many successful iterations were below `slowThresholdMBps`.
  This directly answers "how many iterations were less than 5 MB/s?"
- **`slowRunsPercent`** — same thing as a percentage of successful runs.

## How the speed is measured

- Wall clock time from just before `scp` starts until it returns.
- Divided into the actual on-disk size of the downloaded file.
- Reported in **both** MB/s (megabytes — what most people mean by "67 MB/s")
  and Mbps (megabits — what ISPs quote, 8× higher).
- The downloaded file is deleted after each iteration before the next one
  starts. Files live in `/tmp/speedtest-XXXXX/` which is auto-cleaned at exit.

## Notes / gotchas

- First iteration is sometimes slower (TCP slow-start, ssh handshake). If
  you care, drop iteration 1 when analyzing.
- If B caches the file in RAM after the first read, later runs will look
  faster than the actual network. To force a real network read every time,
  on B periodically run `echo 3 | sudo tee /proc/sys/vm/drop_caches` — or
  just use a file big enough to not fit in RAM.
- Run inside `tmux` / `screen` / `nohup` for long runs so an ssh disconnect
  doesn't kill it:
  ```bash
  tmux new -s speedtest
  ./speedtest
  # Ctrl+B then D to detach; `tmux attach -t speedtest` to come back
  ```