# TeslaBox Architecture

---

# Overall Architecture

```text
                Internet
                    │
     ┌──────────────┼──────────────┐
     │              │              │
 Local AP      Tailscale      Relay Server
     │              │              │
     └──────────────┼──────────────┘
                    │
             TeslaBox HTTP API
                    │
        ┌───────────┼───────────┐
        │           │           │
    TeslaCam      CarPlay      OTA
        │
     Storage
```

See [TeslaUSB Emulation Requirements](TESLAUSB_REQUIREMENTS.md) for the authoritative scope of the first core feature.

---

# Confirmed Hardware Architecture

```text
Tesla dashcam USB port
          │
          │ USB Mass Storage
          ▼
ROCK 3A USB OTG device port
          │
          │ configfs USB Gadget
          ▼
TeslaCam virtual disk image on external 128 GB USB drive

ROCK 3A microSD ── Debian and TeslaBox
```

The microSD card does not store the TeslaCam image, recording snapshots, or media index. The external drive connects through a USB host port and remains a fixed data drive. Moving Debian to NVMe later does not change this data model.

---

# Network Architecture

TeslaBox supports three transport layers.

## AP

Phone
    │
TeslaBox AP
    │
HTTP

---

## Tailscale

Phone
    │
100.x.x.x
    │
TeslaBox

No public IP required.

---

## Relay

TeslaBox
      │
Persistent WebSocket
      │
Relay Server
      │
Browser

No inbound ports required.

---

# Software Layers

```text
┌──────────────────────────────┐
│          Web UI              │
├──────────────────────────────┤
│         REST API             │
├──────────────────────────────┤
│     TeslaBox Services        │
├──────────────────────────────┤
│ USB │ OTA │ CarPlay │ NAS    │
├──────────────────────────────┤
│ Gadget │ Snapshot │ TeslaCam │
├──────────────────────────────┤
│ configfs │ udev │ Linux BSP  │
└──────────────────────────────┘
```

---

# Directory Structure

```text

teslabox/

├── cmd/
│   └── server/
│
├── internal/
│   ├── api/
│   ├── usb/
│   ├── gadget/
│   ├── mount/
│   ├── snapshot/
│   ├── teslacam/
│   ├── network/
│   ├── transport/
│   ├── media/
│   ├── ota/
│   ├── carplay/
│   ├── monitor/
│   └── backup/
│
├── web/
│
├── configs/
│
├── scripts/
│
└── docs/
```

---

# Tesla USB Flow

```text
discover and validate external data drive
          ↓
prepare TeslaCam virtual disk image
          ↓
create Mass Storage Gadget through configfs
          ↓
bind UDC; Tesla writes the virtual disk
          ↓
create a copy-on-write snapshot of the active image
          ↓
check the snapshot filesystem and mount read-only
          ↓
scan completed TeslaCam clips and atomically switch the index
          ↓
REST API / Web UI
```

First-time data-drive initialization is a separate explicit operation. Normal service startup must never repartition or format the drive.

---

# Video Playback

```text
Tesla writes the active image
          │
          └── consistent snapshot ──> completed MP4 ──> HTTP Range ──> browser
```

The web application never reads or mounts the writable image exposed to Tesla. An MP4 still being written is not guaranteed to play. "Record while playing" means recording continues while completed clips are served from a snapshot.

Remote mode:

Read-only snapshot

↓

Hardware Transcoding (MPP)

↓

Low Bitrate Stream

↓

Browser

---

# Transport Layer

Transport Interface

↓

AP Transport

↓

Tailscale Transport

↓

Relay Transport

All transports expose the same HTTP API.

---

# OTA

Cloud

↓

Update Check

↓

Download

↓

Verify

↓

Apply

↓

Rollback

---

# Monitoring

CPU

Memory

Temperature

Storage

Network

Logs

---

# Future

- BLE
- AI Video Search
- Face Recognition
- Cloud Sync
- Plugin System
