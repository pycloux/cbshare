# Installation

cpsync is a single executable with no dependencies. Install it on the machine that will act as the server; the other machines only need a web browser.

## Download

Download the archive for your platform from the [releases page](https://github.com/pycloux/cpsync/releases):

| Platform | Archive |
| --- | --- |
| Windows, 64-bit Intel/AMD | `cpsync-<version>-windows-amd64.zip` |
| Windows on ARM | `cpsync-<version>-windows-arm64.zip` |
| Linux, 64-bit Intel/AMD | `cpsync-<version>-linux-amd64.tar.gz` |
| Linux on ARM (Raspberry Pi 4/5, ARM servers) | `cpsync-<version>-linux-arm64.tar.gz` |

Each release also has a `SHA256SUMS` file to check the download.

=== "Windows"

    1. Extract the zip file, for example to `C:\Tools\cpsync`.
    2. Open PowerShell or Command Prompt in that folder.
    3. Run `.\cpsync.exe --help` to check that it works.

    The first time cpsync listens on the network, Windows Defender Firewall asks whether to allow it. Allow it on **private networks** so that other machines can connect.

=== "Linux"

    ```sh
    tar -xzf cpsync-<version>-linux-amd64.tar.gz
    cd cpsync-<version>-linux-amd64
    ./cpsync --help
    ```

    If a firewall is active, open the port (8443 by default), for example `sudo ufw allow 8443/tcp`.

## Build from source

You need Go 1.24 or later.

```sh
git clone https://github.com/pycloux/cpsync.git
cd cpsync
make build                       # creates ./cpsync
make cross                       # Linux and Windows binaries in dist/
```

Without `make`, run `go build -o cpsync ./cmd/cpsync`.

## Where cpsync keeps its data

Everything lives in one encrypted database file, `cpsync.db` in the current folder by default. Use `--db` to put it elsewhere. Back it up together with the key if you want to keep its content; without the key the file is useless.

Next: [first-time setup](first-start.md).
