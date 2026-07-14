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

# V0.1 Basic Framework

Estimated goal:

Implement automatic Tesla USB detection.

## Features

- [ ] Go project initialization
- [ ] Configuration management
- [ ] Logging module
- [ ] HTTP Server
- [ ] REST API
- [ ] Automatic USB detection (udev)
- [ ] Auto mount
- [ ] Auto unmount
- [ ] TeslaCam detection
- [ ] Video scanning
- [ ] Video list API

After completion:

Users can view TeslaCam file lists via API.

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

# V0.3 Online Playback

Estimated goal:

Support online video playback.

## Features

- [ ] HTTP Range
- [ ] MP4 playback
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

- USB auto detection
- Auto mount
- TeslaCam scanning
- Video API

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

TeslaBox is not meant to replicate TeslaUSB.

TeslaUSB focuses on **USB storage emulation**.

TeslaBox aspires to be:

> An open-source edge computing platform running inside a Tesla vehicle (Tesla Edge Platform).

In the future, it will not only serve TeslaCam but also continuously expand with more capabilities related to the Tesla USB ecosystem.