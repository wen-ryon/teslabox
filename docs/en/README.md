# Features

## Tesla USB

TeslaBox emulates USB Mass Storage to Tesla through the ROCK 3A OTG port. The microSD card runs Debian only; an external 128 GB USB drive stores the TeslaCam virtual disk, snapshots, and media index.

- Browse completed clips from consistent snapshots while Tesla keeps recording
- MP4 files still being written are not guaranteed to play; live camera streaming is out of scope

- Remote TeslaCam browser
- Online TeslaCam playback
- Download TeslaCam videos
- Multi-camera synchronized playback
- Video thumbnails
- Search by date/event
- RecentClips / SavedClips management

See [TeslaUSB Emulation Requirements](TESLAUSB_REQUIREMENTS.md) for the authoritative scope.

Future USB Features:

- Lock Chime Manager
- Light Show Manager
- Vehicle Skin Manager
- Online Resource Store

---

## Network

### AP Router

TeslaBox works as a Wi-Fi 6E Access Point.

- Share 4G/5G mobile network
- Tesla Browser Internet Access
- Mobile Phone Internet Access
- Local TeslaCam Playback

### Remote Access

Support three access modes:

- Local AP
- Tailscale (VPN)
- Public Relay Server

No public IP required.

---

## Wireless CarPlay (Deferred)

Wireless CarPlay is outside the current development scope because it requires a licensed MFi authentication and receiver solution.

---

## OTA

- One-click OTA Update
- Rollback Support
- Incremental Upgrade
- Version Management

---

## Monitoring

- CPU Temperature
- Storage Health
- Memory Usage
- Network Status
- Device Logs

---

## Backup

Automatic backup to:

- SMB NAS
- NFS
- WebDAV
- SFTP

---

# Hardware

## Current Hardware

- Radxa ROCK 3A
- RK3568
- 2GB RAM
- microSD card: Debian and TeslaBox
- External Tesla USB drive (128 GB): virtual disk, snapshots, and media index
- NVMe: optional future Debian system disk

## Expansion

### Wi-Fi

MediaTek MT7922

- Wi-Fi 6
- AP Mode
- Linux Friendly

### Cellular

4G / 5G Module (Planned)

- Mobile Data
- Always Online
- Remote Access

---

# Software Stack

Go

Linux

Systemd

udev

FFmpeg

Rockchip MPP

Tailscale

Hostapd

Dnsmasq

---

# Roadmap

## Phase 1

- [x] Go service, stable data-drive discovery, automatic mount, and status API
- [x] TeslaCam scanner and video API prototypes
- [x] Explicit external data-drive initialization
- [x] TeslaCam backing image
- [x] USB Mass Storage Gadget, restart recovery, and disconnect protection
- [x] Consistent snapshot, read-only mount, and atomic media-index switch (validated with the real image on the board)
- [x] HTTP Range six-camera composition, single-camera playback, and download (validated with a real MP4 on the board)
- [x] Low-space degradation, snapshot failure backoff, and stale-resource recovery after abnormal power loss
- [x] Read-only automated acceptance tool for concurrent recording and playback
- [ ] Concurrent playback while Tesla is writing

## Phase 2

- [ ] Web UI
- [ ] Video Playback
- [ ] Thumbnail
- [ ] Download

## Phase 3

- [ ] AP Router
- [ ] MT7922
- [ ] Tailscale
- [ ] OTA

## Phase 4

- [ ] NAS Backup
- [ ] Resource Center

---

# Hardware Diagram

```text
Tesla dashcam USB port
          │
          │ USB Mass Storage
          ▼
ROCK 3A USB OTG device port
          │
          │ USB Gadget
          ▼
TeslaCam virtual disk image
          │
          ▼
ROCK 3A USB host port ── external 128 GB USB drive

ROCK 3A microSD ── Debian and TeslaBox
ROCK 3A M.2 E-Key ── MT7922 ── Wi-Fi AP
ROCK 3A USB ── 4G/5G modem ── mobile network
```

---

# License

MIT
