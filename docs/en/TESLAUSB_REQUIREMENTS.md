# TeslaUSB Emulation Requirements

This document is the requirements baseline for the first core TeslaBox feature. If another README, architecture document, or development plan conflicts with it, this document takes precedence.

## Goal

TeslaBox replaces a USB drive directly attached to the vehicle. ROCK 3A emulates USB Mass Storage through USB Gadget, while the vehicle records continuously. Users can browse and play completed TeslaCam clips through the TeslaBox web interface without interrupting recording.

"Record while playing" means:

- Vehicle recording remains uninterrupted.
- The web interface plays MP4 files that have been closed and are fully readable.
- An MP4 file that is still being written is not guaranteed to play.
- This is not a live camera stream.

## Confirmed Hardware Architecture

```text
Tesla dashcam USB port
          │
          │ USB Mass Storage
          ▼
ROCK 3A USB OTG device port
          │
          │ Linux USB Gadget
          ▼
TeslaCam virtual disk image
          │
          │ images, snapshots, and index
          ▼
ROCK 3A USB host port ── external 128 GB USB drive

ROCK 3A microSD card ── Debian and the TeslaBox application
```

Constraints:

- The microSD card runs Debian and the application only. It must not store the TeslaCam image, recording snapshots, or media index.
- The external 128 GB USB drive is the current recording data medium.
- Debian may later move to NVMe without changing the TeslaUSB data model or upper-level APIs.
- The external data drive is a fixed component while recording. Removing it is a fault, not a normal workflow.

## Storage Model

The existing TeslaCam partition is not passed through directly. Linux manages the external drive as a backing store:

```text
external 128 GB USB drive
└── reflink-capable Linux filesystem
    ├── images/
    │   └── teslacam.img
    ├── snapshots/
    ├── index/
    └── state/
```

`teslacam.img` contains a Tesla-compatible filesystem and directory layout. USB Gadget exposes this image to the vehicle, so the physical data still resides on the external USB drive.

Rules:

- Data-drive initialization may repartition or format the drive and therefore requires an explicit user action. Service startup must never do this automatically.
- Existing recordings must be backed up before initialization.
- The virtual disk must leave capacity for copy-on-write snapshots and runtime state.
- The settings workflow should use as much physical capacity as practical while reserving at least 5% and no less than 10 GB; snapshot and export operations still enforce an 8 GiB and 5% runtime free-space floor. The MBR-backed image is capped at 2 TB.
- Initialization may create either one exFAT TeslaCam partition or an exFAT TeslaCam partition plus a FAT32 MUSIC partition with a user-selected capacity ratio.
- Linux must never mount the writable image while it is exposed to the vehicle.
- Media indexes, snapshots, and high-write runtime state belong on the external data drive to reduce microSD writes.
- Stable identifiers must locate the data drive, images, snapshots, and mount points; `/dev/sdX` names are not stable identifiers.

Virtual disk size, filesystem type, label, snapshot interval, and retention are deployment parameters whose defaults require board and vehicle testing.

## USB Gadget

TeslaBox manages USB Mass Storage Gadget through configfs:

- Validate UDC, configfs, and the mass-storage function before startup.
- Point the LUN at `teslacam.img`, never at a filesystem mounted by Linux.
- Bind and unbind operations must be idempotent. A service restart within the same system boot may inspect or stop an existing Gadget but must not bind the UDC again.
- Debian must still boot without the data drive, enter `WAITING_FOR_DATA_DRIVE`, and expose no empty LUN without blocking startup indefinitely.
- When an initialized data drive with the configured stable identity is inserted after boot, the system must mount it and revalidate the backing image. If the safe binding window for that boot has ended, Gadget remains unbound until the user reconnects OTG and requests a warm reboot recovery; a fully powered-off boot with OTG pre-connected remains the fallback. This path must never format an unknown or uninitialized drive.
- Do not expose a LUN when the backing image is unavailable or the data drive has failed.
- If the active data drive disconnects while Gadget is running, enter a fault state, make a best effort to unbind the UDC, stop providing the LUN, and emit an explicit alert. After the drive returns, repeat identity, mount, and image validation instead of reusing stale state.
- Allow at most one UDC binding attempt per ROCK 3A `boot_id`, during the first 60 seconds of kernel uptime and the initial service convergence. If uptime cannot be verified, fail closed for new bindings without tearing down an existing bound Gadget. If the host does not complete enumeration within the bounded wait, unbind immediately and prohibit retries from polling, udev, service restarts, or UI actions during that boot.
- A single timeout while reading `lsblk`, configfs, or UDC state must not be treated as a disconnected data drive and must not unbind an already active Gadget.
- After the boot-time binding succeeds, keep the Gadget bound while the vehicle resets or re-enumerates USB. Stop the LUN only when the backing store is unavailable or the active data drive is confirmed missing.

## Consistent Snapshots and Online Viewing

Online viewing reads a consistent snapshot:

```text
vehicle writes teslacam.img
             │
             ├── create copy-on-write snapshot
             ▼
       snapshot.img
             │
             ├── check or repair the snapshot filesystem
             ├── mount read-only
             ├── scan TeslaCam and TeslaTrackMode
             └── serve list, playback, and download APIs
```

Rules:

- Snapshot creation and reading must not unbind the active Gadget.
- A snapshot must be independent of the image still being written.
- Mount snapshots read-only; filesystem repair may only modify the snapshot copy.
- Try a direct read-only mount first. Run `fsck.exfat` only when the snapshot copy cannot be mounted, so periodic scans do not contend with vehicle music reads or recording writes.
- APIs return only media files that are complete and readable in a snapshot.
- Switch the media index atomically after a new snapshot becomes available. Do not delete an old snapshot while it is still serving a playback request.
- Snapshot failure must not interrupt vehicle recording. Keep the last usable snapshot and expose a degraded state.
- Enforce snapshot space and retention limits.

## First-Stage TeslaCam Scope

- `RecentClips`
- `SavedClips`
- `SentryClips`
- `TeslaTrackMode`
- Queries by date, event, and camera
- Single-camera MP4 playback and download
- Storage, Gadget, snapshot, and index status

Later stages add synchronized multi-camera playback, thumbnails, timelines, NAS backup, and Tesla USB resource management.

## State Machine

```text
STARTING
  ↓
WAITING_FOR_DATA_DRIVE
  ↓
PREPARING_BACKING_STORE
  ↓
GADGET_ACTIVE
  ├── SNAPSHOT_CREATING
  ├── SNAPSHOT_READY
  └── DEGRADED

any state ── severe storage fault ──> FAULT
```

State changes must be visible in logs and APIs. Service exits, restarts, repeated udev events, and unexpected power loss must converge without reformatting, duplicate Gadgets, or writable mounts left behind.

Data-drive hotplug must drive state convergence: remain in `WAITING_FOR_DATA_DRIVE` after a diskless boot, mount and validate a matching drive when it appears, but enter `GADGET_ACTIVE` only during a new cold-boot binding window. If the active drive disconnects, enter `FAULT` and stop LUN exposure.

## Safety Boundaries

- Never let Tesla and Linux mount the same writable filesystem concurrently.
- Never format, repartition, or delete recordings implicitly at service startup.
- A destructive initializer must show the stable drive identifier, size, and model and run as a separate explicit operation.
- Do not expose arbitrary device paths, silent formatting, or recording deletion APIs.
- Some ROCK 3A 5.10 kernels retain UDC `configured` and `high-speed` after the OTG cable is physically removed. These values therefore mean that enumeration completed most recently; they do not prove a current physical connection.
- If UDC still reports `configured` while `/sys/class/udc/<UDC>/device/gadget/suspended` is `1`, the UI must report that the cable may be disconnected and explain that a normally suspended host cannot be distinguished losslessly from physical removal on this kernel. It must not present the link as active or rebind automatically merely to probe it, because that would interrupt vehicle writes.
- Before an update, the authenticated same-origin UI may unbind only the TeslaBox-owned Gadget after the user explicitly confirms that the physical OTG cable is already removed. Persist the pause on the system disk across service restarts and updates. After inserting OTG, the user may only schedule recovery for the next cold boot; the current process must remain unbound. The API must not accept an arbitrary Gadget name, bypass confirmation, or automatically rebind after an update failure.
- `/sys/class/extcon/extcon0/state` is not a trustworthy device-mode OTG cable signal on the tested board because it may still report `USB=1` after OTG removal. It must not authorize a bind.
- Report low space, read-only media, I/O errors, and disconnects explicitly.

## First-Stage Acceptance Criteria

1. ROCK 3A boots Debian from microSD without writing TeslaCam data to it.
2. The explicitly initialized external 128 GB USB drive stores the virtual disk image.
3. ROCK 3A exposes stable USB Mass Storage to a test host or Tesla through OTG.
4. TeslaBox creates and read-only mounts a snapshot while the host writes the virtual disk.
5. The API or web interface plays a completed MP4 from a snapshot while Gadget remains connected and writable.
6. Service restart restores snapshot and index state without rebinding Gadget during the same `boot_id`.
7. Drive disconnect, low space, or snapshot failure never triggers automatic formatting or silent data loss.
8. A diskless boot does not block Debian; inserting the initialized data drive after boot mounts and validates it, while Gadget recovery requires a cold boot with OTG already connected.
9. Removing the active data drive is detected, stops LUN exposure, and reports the fault through logs and APIs.

## Out of Scope for the First Stage

- Playing an MP4 that is still being written.
- Low-latency live camera streaming.
- Automatically migrating existing recordings before formatting the original drive.
- Remotely formatting the data drive through the web interface.
- Low-bitrate transcoding for NAS, cloud, or relay access.
