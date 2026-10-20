# ProcDeck

Runs N copies of one program on a Windows server, restarts the ones that
exit, and changes how many run, without opening a cmd window per copy. The
program can be any command: `php.exe service.php`, a compiled Go service,
`node server.js`, or a script. An operator works from one menu window. It also
runs on Linux.

Two variants have the same menu, `procdeck.ini` and behaviour:

| Folder | Runs as | Needs on the server |
| --- | --- | --- |
| [`php/`](php/README.md) | PHP scripts and a `.bat` to double-click | PHP 8.2 or later |
| [`go/`](go/README.md) | `procdeck.sh` that builds and starts it, mainly on Linux VMs (draft) | Go 1.26 or later (or nothing, with a prebuilt binary) |

Pick the PHP variant on a Windows server that already has PHP. Pick the Go
variant on a Linux VM.

## License

MIT, see [LICENSE](LICENSE).
