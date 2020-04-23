# ghec-backup

> ghec-backup

## Install

```sh
$ go get github.com/stoe/ghec-backup
```

## Usage

```
USAGE:
  ghec-backup [OPTIONS]

OPTIONS:
  -c, --config string         Path to config file. Default: .ghec-backup in current directory
  -h, --help                  Print this help.
  -l, --lock                  Lock repositories while backing up. Default: false
  -o, --organization string   Organization on github.com to backup.
  -r, --repository strings    Repository to backup, can be provided multiple times. Default: organization repositories

EXAMPLE:
  $ ghec-backup
```

## License

MIT © [Stefan Stölzle](https://github.com/stoe)
