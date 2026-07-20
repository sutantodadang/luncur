# Raspberry Pi & ARM

luncur ships `linux/arm64` binaries and multi-architecture container images,
so it runs on a Raspberry Pi (or any arm64 board) exactly like on an x86 VPS —
same `luncur up`, same one-binary story.

## What's supported

- **arm64 / aarch64** — Raspberry Pi 4, Pi 5, Pi 400, Compute Module 4, and
  other 64-bit ARM SBCs, running a **64-bit** OS.
- The release publishes an arm64 CLI/server binary
  (`luncur-linux-arm64`) and **multi-arch** images
  (`ghcr.io/sutantodadang/luncur` and `luncur-builder`) covering
  `linux/amd64` + `linux/arm64`. `docker`/K3s pulls the arch that matches the
  node automatically — the tag is the same on every board.

!!! note "64-bit only"
    32-bit ARM (`armv7`, Raspberry Pi OS 32-bit, Pi 3 and earlier on the
    stock OS) is **not** supported: the build toolchain (BuildKit + nixpacks)
    targets arm64. Use a 64-bit OS — Raspberry Pi OS (64-bit) or Ubuntu Server
    arm64 — on a Pi 4/5.

## Requirements

- A Pi 4/5 with **4 GB RAM or more** recommended (2 GB works for a single small
  app; the in-cluster BuildKit builder is the memory-hungry part).
- A 64-bit OS flashed to a decent SD card or, better, a USB SSD.
- K3s prerequisites: on Raspberry Pi OS you must enable cgroup memory —
  append to `/boot/firmware/cmdline.txt` (single line) and reboot:

    ```text
    cgroup_memory=1 cgroup_enable=memory
    ```

## Install

Same as any box — just fetch the arm64 binary:

```sh
curl -sfLo luncur https://github.com/sutantodadang/luncur/releases/latest/download/luncur-linux-arm64
chmod +x luncur
sudo ./luncur up        # installs K3s + luncur, prints admin credentials (save them!)
```

`luncur up` installs K3s, then pulls the arm64 server and builder images from
the multi-arch tags. Everything after that — `luncur project create`,
`git push`, addons, backups — is identical to the [getting started](../getting-started.md)
flow.

## Building apps on ARM

App builds run **in the cluster** (a BuildKit Job on a node), so an app built
on your Pi produces an **arm64** image, ready to run on that same Pi cluster.
Nixpacks and Dockerfile builds both work; the builder image carries the
arch-matched nixpacks binary.

!!! warning "Mixed-arch clusters"
    In a [multi-node](../getting-started.md) cluster that mixes arm64 and amd64
    nodes, an app image is built for the architecture of whichever node ran the
    build Job, and only schedules onto nodes of that architecture. For
    predictable results, keep a cluster single-architecture (an all-Pi cluster,
    or an all-x86 cluster). Cross-building one image for both arches is not done
    today.

## A small home cluster

Add more Pis as agents the usual way — print the join command on the server
and run it on each new Pi:

```sh
luncur node join-command          # on the server: prints the token + URL
# on each new Pi:
sudo ./luncur join <server-url> --token <token>
luncur node ls                    # verify they registered
```

Keep every node on the same 64-bit OS and arch, and you have a tidy,
low-power PaaS you can hold in one hand.

**Related:** [Build pipeline](../reference/build-pipeline.md) · [Getting started](../getting-started.md)
