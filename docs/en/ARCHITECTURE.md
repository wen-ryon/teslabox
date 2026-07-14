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
│ Mount │ TeslaCam │ Network   │
├──────────────────────────────┤
│ udev │ systemd │ Linux BSP   │
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
│   ├── mount/
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

Tesla USB Inserted

↓

udev Event

↓

USB Manager

↓

Filesystem Check

↓

Auto Mount

↓

TeslaCam Detection

↓

Media Scan

↓

Thumbnail

↓

Metadata

↓

REST API

↓

Web UI

---

# Video Playback

Tesla USB

↓

Original MP4

↓

HTTP Range

↓

Browser

Remote mode:

Tesla USB

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