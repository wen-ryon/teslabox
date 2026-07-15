# TeslaBox Development Plan

> This document records the development roadmap for TeslaBox.
>
> This project adopts an **iterative development** model. Each version should be runnable, testable, and releasable, rather than developing all features at once.

---

# Project Goal

TeslaBox is not just a TeslaCam viewer, but a Linux-based Tesla Edge Platform.

The ultimate goals include:

- Tesla USB Expansion
- Online TeslaCam Viewing
- AP Router
- 4G / 5G Network
- Wireless CarPlay
- OTA Updates
- NAS Auto Backup
- Online Resource Center
- Plugin System

---

# Development Principles

## Iterate in Small Steps

Each version should be:

- Compilable
- Runnable
- Testable
- Releasable

Avoid developing multiple large features at once.

---

## Linux First

Make full use of existing Linux capabilities, such as:

- udev
- systemd
- hostapd
- dnsmasq
- NetworkManager (TBD)

Business logic is implemented in Go, system capabilities are handled by Linux.

---

## Modular Design

Each functional module should be as independent as possible, for example:

- USB
- TeslaCam
- OTA
- CarPlay
- Backup

They should be loosely coupled with each other.

---

# V0.1 TeslaUSB Core Loop

Estimated goal:

Use the external 128 GB USB drive as the backing store, emulate USB Mass Storage to Tesla through OTG, and read completed clips from snapshots while vehicle recording continues.

See [TeslaUSB Emulation Requirements](TESLAUSB_REQUIREMENTS.md) for the hardware, storage, and consistency boundaries.

## Features

- [x] Go project initialization
- [x] Configuration management
- [x] Logging module
- [x] HTTP Server
- [x] REST API
- [x] TeslaCam parser and video-list API prototype
- [x] Discover the external data drive by UUID or label
- [x] Read-only backing store, configfs, and UDC preflight
- [x] Starting, waiting, preparing, ready, degraded, and fault state model
- [x] Data-drive, backing-store, and Gadget-environment status API
- [x] Separate, explicit, and guarded data-drive initializer
- [x] Reflink-capable backing store
- [ ] Create and format the TeslaCam virtual disk image
- [ ] configfs USB Mass Storage Gadget
- [ ] Idempotent Gadget bind, unbind, and restart recovery
- [ ] Copy-on-write snapshot, filesystem check, and read-only mount
- [ ] Atomic media-index switch between snapshots
- [ ] HTTP Range, single-camera MP4 playback, and download
- [ ] Active Gadget, snapshot, and index status APIs
- [ ] Degraded handling for drive disconnect, low space, and snapshot failure

After completion:

Users can play completed TeslaCam clips from a snapshot while Tesla keeps recording and Gadget stays connected.

---

# V0.2 Web Management Console

Estimated goal:

Implement basic Web UI.

## Features

- [ ] Login page
- [ ] Dashboard
- [ ] TeslaCam browsing
- [ ] Video details
- [ ] Video download
- [ ] Video thumbnails
- [ ] Video search
- [ ] System status page

After completion:

Users can view TeslaCam through a browser.

---

# V0.3 Advanced Playback

Estimated goal:

Complete the synchronized multi-camera playback experience.

## Features

- [ ] Four-channel synchronized playback
- [ ] Timeline synchronization
- [ ] Fast forward
- [ ] Seek playback
- [ ] Full screen

Future:

Support low bitrate playback.

---

# V0.4 AP Router

Estimated goal:

TeslaBox becomes a router.

## Features

- [ ] MT7922
- [ ] AP mode
- [ ] hostapd
- [ ] dnsmasq
- [ ] DHCP
- [ ] NAT
- [ ] Web configuration page
- [ ] Client management

After completion:

Phones and Teslas can connect to TeslaBox Wi-Fi.

---

# V0.5 Remote Access

Estimated goal:

Support remote access.

## Features

Three access methods supported:

### Local AP

Phone connects to TeslaBox hotspot.

### Tailscale

No public IP required.

### Relay Server

Traffic forwarding through a public server.

After completion:

View TeslaCam from anywhere.

---

# V0.6 4G / 5G

Estimated goal:

Completely independent from phone hotspot.

## Features

- [ ] USB Dongle
- [ ] M.2 Module (TBD)
- [ ] Auto dial
- [ ] Network switching
- [ ] Signal strength
- [ ] Traffic statistics

---

# V0.7 OTA

Estimated goal:

Support remote upgrade.

## Features

- [ ] Check for updates
- [ ] Download updates
- [ ] Verification
- [ ] Automatic upgrade
- [ ] Rollback

---

# V0.8 NAS Auto Backup

Estimated goal:

Automatically sync TeslaCam.

## Supported

- SMB
- NFS
- SFTP
- WebDAV

Supports:

- Auto sync
- Scheduled sync
- Incremental sync

---

# V0.9 Wireless CarPlay

Estimated goal:

Run CarPlay in Tesla browser.

## Features

- [ ] Wireless CarPlay
- [ ] Auto connect
- [ ] Audio forwarding
- [ ] Full screen mode

---

# V1.0 Tesla Resource Center

Estimated goal:

Build a Tesla USB resource platform.

## Features

Online download:

- Lock Chimes
- Light Shows
- Vehicle Skins
- USB Resources

Supports:

- One-click install
- One-click restore

---

# V1.1 Plugin System

Estimated goal:

Open up extensibility.

Plugins can add:

- New pages
- New APIs
- New device support

---

# V2.0 AI

Long-term plan.

May include:

- AI video search
- AI event recognition
- AI parking analysis
- AI driving statistics

---

# Current Priorities

⭐⭐⭐⭐⭐

- Safe external data-drive initialization
- TeslaCam backing image
- USB Mass Storage Gadget
- Consistent snapshots
- Play completed clips while vehicle recording continues

⭐⭐⭐⭐

- Web UI
- Online playback
- Video thumbnails

⭐⭐⭐

- AP Router
- Tailscale
- OTA

⭐⭐

- NAS
- Resource Center

⭐

- AI

---

# Development Philosophy

TeslaBox does not copy TeslaUSB scripts line by line. It adopts reliable **USB storage emulation, snapshots, and failure recovery** as its first core capability, while rebuilding the business state machine and management APIs in Go.

TeslaBox aspires to be:

> An open-source edge computing platform running inside a Tesla vehicle (Tesla Edge Platform).

In the future, it will not only serve TeslaCam but also continuously expand with more capabilities related to the Tesla USB ecosystem.
