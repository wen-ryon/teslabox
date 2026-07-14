# Features

## Tesla USB

- Remote TeslaCam browser
- Online TeslaCam playback
- Download TeslaCam videos
- Multi-camera synchronized playback
- Video thumbnails
- Search by date/event
- RecentClips / SavedClips management

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

## Wireless CarPlay

Wireless CarPlay Receiver running on Linux.

Tesla Browser launches CarPlay Web UI automatically.

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
- microSD / NVMe
- Original Tesla USB Drive (128GB)

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

- [ ] Tesla USB Detection
- [ ] Auto Mount
- [ ] TeslaCam Scanner
- [ ] Video API

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

- [ ] Wireless CarPlay
- [ ] NAS Backup
- [ ] Resource Center

---

# Hardware Diagram

```text
            Tesla
              │
              │
       Tesla USB Drive
              │
              ▼
        Radxa ROCK 3A
              │
   ┌──────────┴──────────┐
   │                     │
MT7922              4G/5G Module
   │                     │
AP WiFi           Mobile Internet
   │                     │
   └──────────┬──────────┘
              │
           TeslaBox
```

---

# License

MIT