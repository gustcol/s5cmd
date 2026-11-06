# Changelog
## v2.5.0 - unreleased

#### Features
- New TLS options `--ca-cert`, `--client-cert` and `--client-key` for private CAs and mutual TLS. The CA file replaces the system roots (like `AWS_CA_BUNDLE` and `curl --cacert`) and wins over `AWS_CA_BUNDLE`. `--client-cert` and `--client-key` must be given together. `--no-verify-ssl` works as before.
- New `--transport auto|http1|http2|http3` option (env `S5CMD_TRANSPORT`, default `auto`). `auto` uses HTTP/1.1, `http1` and `http2` restrict the protocol, and `http2` fails when the server does not negotiate h2. The negotiated protocol is logged at debug level (trace level for `auto`). `http3` needs a build with the `http3` tag and fails in the default build.
- New `--transport-conns N` option (env `S5CMD_TRANSPORT_CONNS`, default 8) to spread `--transport http2` over several connections. Go uses one HTTP/2 connection per host, which limits bulk copies.
- New `--unsigned-payload` option. Uploads over https send `UNSIGNED-PAYLOAD` instead of a SHA-256 of the body, so the body is protected by TLS only. Ignored for plain http endpoints.
- New `--no-content-md5` option. Uploads do not send a `Content-MD5` header, so the server does not check the body against it. With `--unsigned-payload` a 4 GiB upload over TLS used 1.8 s instead of 13.5 s of client CPU.

#### Improvements
- `--transport auto` uses HTTP/1.1. Go negotiated HTTP/2 on its own with TLS front ends and sent everything over one connection, which made 4 GiB copies 4-5x slower than HTTP/1.1.
- `run` files with many `cp` and `mv` lines use about 23% less CPU, because the command text is built only when an error is printed.

#### Bugfixes
- The CA bundle from `AWS_CA_BUNDLE` or the shared AWS config is also used when `--transport http2` opens several connections.
- `AWS_CA_BUNDLE` without `--ca-cert` no longer breaks `--transport auto` and `http1` against servers that offer HTTP/2 (every request failed with "malformed HTTP response").

## v2.4.0 - 2026-11-06

This is the first release of the maintained fork at github.com/gustcol/s5cmd. Building from source needs Go 1.26 or newer.

#### Breaking changes
- `DEBUG` and `TRACE` log messages are now written to stderr instead of stdout, so they no longer mix with command output. ([#851](https://github.com/peak/s5cmd/issues/851))
- `sync` now exits with 1 when listing or planning fails (wrong object name, empty wildcard, access denied, sort failure). It used to print the error and exit with 0. Syncing from an empty source with `--delete` still deletes the destination objects, but now exits with 1 because of the "no object found" error. ([#869](https://github.com/peak/s5cmd/issues/869))
- `ls --humanize` and `du --humanize` print a `B` suffix for sizes below 1K (`123B` instead of `123`). ([#817](https://github.com/peak/s5cmd/issues/817)) ([#884](https://github.com/peak/s5cmd/pull/884)) Terry Rankine
- The minimum Go version to build from source is 1.26. The Docker image builds with Go 1.27.

#### Security
- `cp`, `mv` and `sync` refuse S3 keys that would be written outside a local destination (for example `data/../../evil`). The object is refused with an error, the other objects are still processed, and the command exits non-zero. On Windows, keys with backslash-separated `..` segments or a colon, reserved device names (`NUL`, `COM1`) and names ending in a dot or space are refused too. A directory inside the destination that is a symlink to a place outside it is not followed either. ([#872](https://github.com/peak/s5cmd/issues/872))

#### Bug fixes
- `sync --delete` no longer deletes destination objects that are excluded by `--exclude` or not matched by `--include`. ([#815](https://github.com/peak/s5cmd/issues/815)) ([#883](https://github.com/peak/s5cmd/pull/883)) Terry Rankine
- `sync` uses `--source-region` and `--destination-region` when listing the source and the destination. ([#816](https://github.com/peak/s5cmd/issues/816))
- `sync --delete` with `--exclude` or `--include` also works when the local destination is spelled `./dst/`, `./dst` or `dst/./`. Before, excluded files in such a destination were deleted.
- `sync --delete` deletes nothing and exits non-zero with "not deleting: source listing incomplete" when listing the source failed part way (for example an unreadable directory). An empty source still deletes as before.
- `cp --if-source-newer` skips the copy when the modification time of the source or the destination is unknown, instead of overwriting.
- `sync --exit-on-error` stops the whole sync when an object key is refused.
- `--no-verify-ssl` keeps the dial timeout, keep-alive and HTTP/2 settings of the default transport.
- A second Ctrl-C (or SIGTERM) quits right away. The first one still starts a clean shutdown.
- `rm` can delete objects whose key ends with `/` (directory markers), with `--raw` or with a wildcard. ([#834](https://github.com/peak/s5cmd/issues/834))
- `cp --no-clobber` checks the destination in the region given by `--destination-region`. ([#839](https://github.com/peak/s5cmd/issues/839))
- `s5cmd` terminates itself with SIGINT or SIGTERM after cleanup, so shells and supervisors can see it was interrupted. On Windows it exits with 130. ([#615](https://github.com/peak/s5cmd/issues/615))
- `sync` compiles against newer `lanrat/extsort` releases (the sort output channels are receive-only). ([#835](https://github.com/peak/s5cmd/issues/835))
- `cp` and `sync` return an error on "too many open files" instead of calling `os.Exit` from a worker, so the progress bar, the log and the `--stat` summary are finished and `run` is not killed. ([#882](https://github.com/peak/s5cmd/pull/882)) Terry Rankine
- `sync` no longer leaks an `os.Exit` goroutine, calls the cancel function it creates, does not block the planner when `Run` returns early, and skips nil errors sent by the sorter on cancelation. ([#882](https://github.com/peak/s5cmd/pull/882)) Terry Rankine
- `sync` does not print a second "context canceled" error after the first listing error with `--exit-on-error`.
- Fixed nil-pointer panics in `Copy` with `--no-such-upload-retry-count`, in `Stat`, in `--if-source-newer` when the modification time is missing, and in `parallel.Run` before `Init`, which now panics with a clear message instead of a nil dereference. User-defined metadata no longer drops the retry ID. The "expected at most N arguments" error reports the right number. ([#880](https://github.com/peak/s5cmd/pull/880)) Terry Rankine
- Fixed data races in the progress bar counters and the statistics maps, and a double close of the log channel. ([#881](https://github.com/peak/s5cmd/pull/881)) Terry Rankine
- `--dry-run` returns a usable file handle instead of an empty `os.File` that panicked. ([#879](https://github.com/peak/s5cmd/pull/879)) Terry Rankine
- Errors from endpoint parsing and region lookup are wrapped with `%w`, so `errors.Is` and `errors.As` work. ([#885](https://github.com/peak/s5cmd/pull/885)) Terry Rankine
- A panic in a worker task is reported as a one-line error instead of crashing the process in the middle of other transfers. The stack is printed at the debug log level.
- Canceling (SIGINT, `--exit-on-error`) stops queued tasks from starting in `cp`, `select` and `run`, and the `run` line reader no longer blocks forever when the consumer leaves.

#### Improvements
- As a fallback, the soft limit of open files is raised to the hard limit (capped at 65536) where the Go runtime has not already done it, for example on macOS, so the default 256 workers do not hit "too many open files".
- The HTTP idle connection pool is sized to `--numworkers` x `--concurrency` (capped at 2048) instead of 2 per host.
- Added `benchmark/` script that runs a `--numworkers` x `--concurrency` matrix for many small and a few large files.
- Updated all dependencies. `urfave/cli` stays at v2.19.3 because newer versions break the e2e suite.

#### Build and CI
- GitHub Actions are pinned to commit SHAs.
- CI runs build and tests on Linux, macOS and Windows with Go 1.26 and 1.27, and QA on Linux. The MinIO service container is started from a step.
- The release workflow publishes to this repository's GitHub Releases only. The GCS test job, the Homebrew tap and the Docker Hub workflow, which targeted upstream accounts, are removed.
- `.goreleaser.yml` uses config version 2.
- Staticcheck skips SA1019, because aws-sdk-go v1 is deprecated as a whole.
- The e2e tests no longer pass non-constant format strings to printf-like helpers (new `go vet` rule).

## v2.3.0 - 16 Dec 2024

#### Breaking changes
- Changed the exit code from 1 to 0 for `ls` when used with an empty bucket. Exits with 1 if the bucket is non-existent. ([#722](https://github.com/peak/s5cmd/issues/722)) [@Z9n2JktHlZDmlhSvqc9X2MmL3BwQG7tk](https://github.com/Z9n2JktHlZDmlhSvqc9X2MmL3BwQG7tk)

#### Features
- Added prefix and wildcard support to `cat` command. ([#716](https://github.com/peak/s5cmd/issues/716)) [@occasionallydavid](https://github.com/occasionallydavid)
- Added `head` command. ([#682](https://github.com/peak/s5cmd/issues/682)) [@yitzhaklevi](https://github.com/yitzhaklevi)
- Added go 1.22 support. ([#764](https://github.com/peak/s5cmd/pull/764)) [@lizzzcai](https://github.com/lizzzcai)
- Added `meta-directive` flag to `cp` command. ([#666](https://github.com/peak/s5cmd/issues/666) [#711](https://github.com/peak/s5cmd/issues/711)) [@GordonGustafson](https://github.com/GordonGustafson) [@arosu](https://github.com/arosu)

#### Improvements
- Upgraded alpine base image to 3.18. ([#661](https://github.com/peak/s5cmd/pull/661)) [@embik](https://github.com/embik)
- Upgraded aws-sdk-go to v1.44.298. ([#684](https://github.com/peak/s5cmd/pull/684)) [@matan129](https://github.com/matan129)
- Upgraded lanrat/extsort to v1.0.2 ([#717](https://github.com/peak/s5cmd/issues/717)) [@gkowarzyk](https://github.com/gkowarzyk)
- Changed signaling channels by replacing `chan bool` with `chan struct{}` for efficiency. ([#733](https://github.com/peak/s5cmd/pull/733))

#### Bugfixes
- Fixed the `cp` command to work with the `--content-type` flag when performing a copy operation from S3 to S3. ([#738](https://github.com/peak/s5cmd/issues/738))
- Fixed a bug in `sync` command where objects in Glacier storage at the destination were being overwritten during synchronization ([#712](https://github.com/peak/s5cmd/issues/712)) [@stevenmcastano](https://github.com/stevenmcastano)
- Fixed a bug in `sync` command where source objects with absolute paths failed to match destination paths, causing unnecessary copying during each sync operation.([#676](https://github.com/peak/s5cmd/issues/676)) [@bounlu](https://github.com/bounlu)

## v2.2.2 - 13 Sep 2023

#### Bugfixes
- Fixed `cp` and `pipe` to not omit some of the metadata flags. ([#657](https://github.com/peak/s5cmd/issues/657))
## v2.2.1 - 23 Aug 2023

#### Bugfixes
- Fixed incorrect `s5cmd version` output ([#650](https://github.com/peak/s5cmd/pull/650))
## v2.2.0 - 21 Aug 2023

#### Features
- Added `pipe` command. ([#182](https://github.com/peak/s5cmd/issues/182))
- Added `presign` command. ([#634](https://github.com/peak/s5cmd/pull/634)) [@zemul](https://github.com/zemul)
- Added file types to `select` queries with more range of options to set during the query. ([#494](https://github.com/peak/s5cmd/issues/494))
- Added `--content-disposition` flag to `cp` command. ([#569](https://github.com/peak/s5cmd/issues/569))
- Added `--show-fullpath` flag to `ls` command. ([#596](https://github.com/peak/s5cmd/issues/596))
- Added `--show-progress` flag to `cp` command. ([#51](https://github.com/peak/s5cmd/issues/51))
- Added `--metadata` flag to `cp` and `pipe` commands to set arbitrary metadata for the objects. ([#537](https://github.com/peak/s5cmd/issues/537))
- Added `--include` flag to `cp`, `rm`, and `sync` commands. ([#516](https://github.com/peak/s5cmd/issues/516))
- Added `--content-disposition` flag to `cp` command. ([#569](https://github.com/peak/s5cmd/issues/569))


#### Improvements
- Implemented concurrent multipart download support for `cat` command. ([#245](https://github.com/peak/s5cmd/issues/245))
- Upgraded minimum required Go version to 1.19. ([#583](https://github.com/peak/s5cmd/pull/583))
- `ListObjectsV2` S3 API is enabled for Google Cloud Storage. ([#617](https://github.com/peak/s5cmd/pull/617))
- Added installation instructions for FreeBSD. ([#573](https://github.com/peak/s5cmd/pull/573)) [@ehaupt](https://github.com/ehaupt)
- Added `ppc64le` support. ([#552](https://github.com/peak/s5cmd/pull/552)) [@mgiessing](https://github.com/mgiessing)

#### Bugfixes
- Fixed a bug that causes `sync` command with whitespaced flag value to fail. ([#541](https://github.com/peak/s5cmd/issues/541)) [ataberkgrl](https://github.com/ataberkgrl)
- Fixed a bug introduced with `external sort` support in `sync` command which prevents `sync` to an empty destination with `--delete` option. ([#576](https://github.com/peak/s5cmd/issues/576))
- Fixed a bug in `sync` command, which previously caused the command to continue running even if an error was received from the destination bucket. ([#564](https://github.com/peak/s5cmd/issues/564))
- Fixed a bug that causes local files to be lost if downloads fail. ([#479](https://github.com/peak/s5cmd/issues/479))
- Fixed a bug where `cp` command could not upload a non-regular file to remote destination. ([#618](https://github.com/peak/s5cmd/pull/618))
- Fixed a crash where a file or a remote object is removed or renamed after it is listed to be operated on. ([#620](https://github.com/peak/s5cmd/pull/620))

## v2.1.0 - 19 Jun 2023

#### Breaking changes
- Adjacent slashes in key are no longer removed when uploading to remote. Before `s5cmd cp file.txt s3://bucket/a//b///c/` would copy to `s3://bucket/a/b/c/file.txt` but now to `s3://bucket/a//b///c/file.txt`.([#459](https://github.com/peak/s5cmd/pull/459))
- `--endpoint-url` will not accept URLs without scheme such as `example.com`. Instead, it will give an error and ask for an url with a scheme; either `http://example.com` or `https://example.com` ([#496](https://github.com/peak/s5cmd/pull/496)).

#### Features
- Added `--content-type` and `--content-encoding` flags to `cp` command. ([#264](https://github.com/peak/s5cmd/issues/264))
- Added `--profile` flag to allow users to specify a [named profile](https://docs.aws.amazon.com/cli/latest/userguide/cli-configure-profiles.html). ([#353](https://github.com/peak/s5cmd/issues/353))
- Added `--credentials-file` flag to allow users to specify path for the AWS credentials file instead of using the [default location](https://docs.aws.amazon.com/cli/latest/userguide/cli-configure-files.html#cli-configure-files-where).
- Added `--all-versions` flag to `ls`, `rm`, `du` and `select` subcommands to apply operation on(/over) all versions of the objects. ([#475](https://github.com/peak/s5cmd/pull/475))
- Added `--version-id` flag to `cat`, `cp`/`mv`, `rm`, `du`  and `select` subcommands to apply operation on(/over) a specific versions of the object. ([#475](https://github.com/peak/s5cmd/pull/475))
- Added `bucket-version` command to configure bucket versioning. Bucket name alone returns the bucket versioning status of the bucket. Bucket versioning can be configured with `set` flag. ([#475](https://github.com/peak/s5cmd/pull/475))
- Added `--raw` flag to `cat` and `select` subcommands. It disables the wildcard operations. ([#475](https://github.com/peak/s5cmd/pull/475))
- Added `bench.py` script under new `benchmark` folder to compare performances of two different builds of s5cmd. ([#471](https://github.com/peak/s5cmd/pull/471))

#### Improvements
- Disable AWS SDK logger if log level is not `trace`. ([#460](https://github.com/peak/s5cmd/pull/460))
- Allow adjacent slashes to be used as keys when uploading to remote. ([#459](https://github.com/peak/s5cmd/pull/459))
- Debian packages are provided on [releases page](https://github.com/peak/s5cmd/releases) ([#380](https://github.com/peak/s5cmd/issues/380))
- Upgraded minimum required Go version to 1.17.
- The sync command uses `external sort` instead of `internal` sort. This change
 reduces RAM usage from ~10 GB to ~1.5 GB for `sync` operation of a directory containing
 1,000,000 files at a cost of speed (20% slower for 1,000,000 objects). For smaller
 directories (~50,000 files) there is no significant change in speed.  ([#483](https://github.com/peak/s5cmd/pull/483))
- Improve auto-completion support of s5cmd for `zsh` and `bash`, start supporting `pwsh` and stop the support for `fish`. Now s5cmd can complete bucket names, s3 keys in a bucket and the local files. However, `install-completion` flag no longer _installs_ the completion script to `*rc` files instead it merely gives instructions to install autocompletion and provides the autocompletion script ([#500](https://github.com/peak/s5cmd/pull/500)).

#### Bugfixes
- Fixed a bug where (`--stat`) prints unnecessarily when used with help and version commands ([#452](https://github.com/peak/s5cmd/issues/452))
- Changed cp error message to be more precise. "given object not found" error message now will also include absolute path of the file. ([#463](https://github.com/peak/s5cmd/pull/463))
- Fixed a bug where some part of the destination path is removed by `cp` and `sync` subcommands ([#360](https://github.com/peak/s5cmd/issues/360))
- Fixed a bug where proxy is not being used when `--no-verify-ssl` flag is used. ([#445](https://github.com/peak/s5cmd/issues/445))
- Fixed `unknown url format` error when object key also includes `s3://` e.g. `s5cmd ls s3://foo/bar/s3://baz` ([#449](https://github.com/peak/s5cmd/issues/449))
- Fixed a bug where the local file created for the download operation was not deleted if the download fails in Windows. ([#348](https://github.com/peak/s5cmd/issues/348))

## v2.0.0 - 4 Jul 2022

#### Breaking changes
- Dropped inline comment feature for `run` command. Previously s5cmd supported a command with an inline comment like `ls s3://bucket/object.gz  # inline comment`. ([#309](https://github.com/peak/s5cmd/issues/309))
- Changed homebrew installation command on macOS. Users can install s5cmd via `brew install peak/tap/s5cmd`. ([#356](https://github.com/peak/s5cmd/issues/356))
- Print usage errors to stderr instead of stdout and do not show help text on usage error. ([#399](https://github.com/peak/s5cmd/issues/399))
- Working directory of the Docker image changed to `/aws` from `/`. ([#412](https://github.com/peak/s5cmd/pull/412))

#### Features
- Added `sync` command to synchronize two given buckets, prefixes, or objects. ([#3](https://github.com/peak/s5cmd/issues/3))
- Added AWS Single Sign-On (SSO) profiles support. ([#385](https://github.com/peak/s5cmd/issues/385))
- Added `--force-glacier-transfer` flag to `select` command. ([#346](https://github.com/peak/s5cmd/issues/346))
- Added `--ignore-glacier-warnings` flag to `cp`, `mv` and `select` commands. ([#346](https://github.com/peak/s5cmd/issues/346))
- Added `--request-payer` flag to include `x-amz-request-payer` in header while sending GET, POST and HEAD requests. ([#297](https://github.com/peak/s5cmd/issues/297)) [@Kirill888](https://github.com/Kirill888)
- Added `--use-list-objects-v1` flag to force using S3 ListObjects API instead of ListObjectsV2 API. ([#405](https://github.com/peak/s5cmd/issues/405)) [@greenpau](https://github.com/greenpau)
- Added trace log level(`--log=trace`) which enables SDK debug logs.([#363](https://github.com/peak/s5cmd/issues/363))

#### Improvements
- Upgraded minimum required Go version to 1.16.

#### Bugfixes
- Fixed a bug about precedence of region detection, which auto region detection would always override region defined in environment or profile. ([#325](https://github.com/peak/s5cmd/issues/325))
- Fixed a bug where errors did not result a non-zero exit code. ([#304](https://github.com/peak/s5cmd/issues/304))
- Print error if the commands file of `run` command is not accessible. ([#410](https://github.com/peak/s5cmd/pull/410))
- Updated region detection call to use current session's address resolving method ([#314](https://github.com/peak/s5cmd/issues/314))
- Fixed a bug where lines with large tokens fail in `run` command. `sync` was failing when it finds multiple files to remove. ([#435](https://github.com/peak/s5cmd/issues/435), [#436](https://github.com/peak/s5cmd/issues/436))
- Print usage error if given log level(`--log`) is not valid. ([#430](https://github.com/peak/s5cmd/pull/430))
- Fixed a bug where (`--stat`) is ignored when log level is error. ([#359](https://github.com/peak/s5cmd/issues/359))

## v1.4.0 - 21 Sep 2021

#### Features

- Added `select` command. It allows to select JSON records from objects using SQL expressions. ([#299](https://github.com/peak/s5cmd/issues/299)) [@skeggse](https://github.com/skeggse)
- Added `rb` command to remove buckets. ([#303](https://github.com/peak/s5cmd/issues/303))
- Added `--exclude` flag to `cp`, `rm`, `ls`, `du` and `select` commands. This flag allows users to exclude objects with given pattern. ([#266](https://github.com/peak/s5cmd/issues/266))
- Added `--raw` flag to `cp` and `rm` commands. It disables the wildcard operations. It is useful when an object contains glob characters which interfers with glob expansion logic. ([#235](https://github.com/peak/s5cmd/issues/235))
- Added `--cache-control` and `--expires` flags to `cp` and `mv` commands. It adds support for setting cache control and expires header to S3 objects. ([#318](https://github.com/peak/s5cmd/pull/318)) [@tombokombo](https://github.com/tombokombo)
- Added `--force-glacier-transfer` flag to `cp` command. It forces a transfer request on all Glacier objects. ([#206](https://github.com/peak/s5cmd/issues/206))
- Added `--source-region` and `destination-region` flags to `cp` command. It allows overriding bucket region. ([#262](https://github.com/peak/s5cmd/issues/262)) [@kemege](https://github.com/kemege)

#### Improvements

- Added `MacPorts` installation option. ([#311](https://github.com/peak/s5cmd/pull/311)) [@manojkarthick](https://github.com/manojkarthick)
- Added `S3_ENDPOINT_URL` environment variable ([#343](https://github.com/peak/s5cmd/pull/343)) [@Dexus](https://github.com/Dexus)
- Prevent retries if a token related error is received ([#337](https://github.com/peak/s5cmd/pull/337))

#### Bugfixes

- Change the order of precedence in URL expansion in file system. Glob (*) expansion have precedence over directory expansion. ([#322](https://github.com/peak/s5cmd/pull/322))
- Fixed data race for concurrent writes for expand ([#330](https://github.com/peak/s5cmd/pull/330))
- Fixed concurrent writes to the flags list of run command ([#335](https://github.com/peak/s5cmd/pull/335))
- Fixed options usage on mv command ([#338](https://github.com/peak/s5cmd/pull/338))

## v1.3.0 - 1 Jul 2021

#### Features

- Added global `--no-sign-request` flag. API requests won't be signed and credentials won't be used if this option is provided. It is useful for accessing public buckets. ([#285](https://github.com/peak/s5cmd/issues/285))

#### Improvements

- If retryable errors are received during command execution, users now can see what's happening under the hood. ([#261](https://github.com/peak/s5cmd/pull/261))
- Update documentation about the AWS_PROFILE environment variable. ([#275](https://github.com/peak/s5cmd/pull/275)) [@davebiffuk](https://github.com/davebiffuk)

#### Bugfixes

- Fixed a bug where write-bit was required to upload a file. ([#258](https://github.com/peak/s5cmd/issues/258))
- Fixed a bug where object could not be found if S3 key contains certain special characters. ([#279](https://github.com/peak/s5cmd/issues/279)) [@khacminh](https://github.com/khacminh)
- `s5cmd` exits with code `1` if given command is not found. It was `0` before. ([#295](https://github.com/peak/s5cmd/issues/295))


## v1.2.1 - 3 Dec 2020

#### Improvements

- Statically link `s5cmd` in Docker image ([#250](https://github.com/peak/s5cmd/issues/250))

#### Bugfixes

- Fixed a bug where HeadBucket request fails during region information retrieval. ([#251](https://github.com/peak/s5cmd/issues/251), [#252](https://github.com/peak/s5cmd/issues/252))


## v1.2.0 - 5 Nov 2020

With this release, `s5cmd` automatically determines region information of destination buckets.

#### Features

- Added global `--dry-run` option. It displays which command(s) will be executed without actually having a side effect. ([#90](https://github.com/peak/s5cmd/issues/90))
- Added `--stat` option for `s5cmd` and it displays program execution statistics before the end of the program output. ([#148](https://github.com/peak/s5cmd/issues/148))
- Added cross-region transfer support. Bucket regions are inferred, thus, supporting cross-region transfers and multiple regions in batch mode. ([#155](https://github.com/peak/s5cmd/issues/155))

#### Bugfixes

- Fixed incorrect MIME type inference for `cp`, give priority to file extension for type inference. ([#214](https://github.com/peak/s5cmd/issues/214))
- Fixed error reporting issue, where some errors from the `ls` operation were not printed.

#### Improvements

- Requests to different buckets not allowed in `rm` batch operation, i.e., throw an error.
- AWS S3 `RequestTimeTooSkewed` request error was not retryable before, it is now. ([205](https://github.com/peak/s5cmd/issues/205))
- For some operations errors were printed at the end of the program execution. Now, errors are displayed immediately after being detected. ([#136](https://github.com/peak/s5cmd/issues/136))
- From now on, docker images will be published on Docker Hub. ([#238](https://github.com/peak/s5cmd/issues/238))
- Changed misleading 'mirroring' examples in the help text of `cp`. ([#213](https://github.com/peak/s5cmd/issues/213))


## v1.1.0 - 22 Jul 2020

With this release, Windows is supported.

#### Breaking changes

- Dropped storage class short codes display from default behaviour of `ls` operation. Instead, use `-s` flag with `ls`
to see full names of the storage classes when listing objects.


#### Features

- Added Server-side Encryption (SSE) support for mv/cp operations. It uses customer master keys (CMKs) managed by AWS Key Management Service. ([#18](https://github.com/peak/s5cmd/issues/18))
- Added an option to show full form of [storage class](https://aws.amazon.com/s3/storage-classes/) when listing objects. ([#165](https://github.com/peak/s5cmd/issues/165))
- Add [access control lists (ACLs)](https://docs.aws.amazon.com/AmazonS3/latest/dev/acl-overview.html)
support to enable managing access to buckets and objects. ([#26](https://github.com/peak/s5cmd/issues/26))


#### Bugfixes

- Fixed infinite repetition issue on mv/cp operations which would occur
 if the destination matched the source wildcard. ([#168](https://github.com/peak/s5cmd/issues/168))
- Fixed windows filepath issue, where backslashes should be treated as the path delimiter. ([#178](https://github.com/peak/s5cmd/issues/178))
- All tests pass on windows, by converting and treating file paths to UNIX filepath format.
- Fixed a transfer issue where the object path contains particular regex metacharacters. ([#111](https://github.com/peak/s5cmd/pull/111)) [@brendan-matroid](https://github.com/brendan-matroid)
- Correctly parse object paths that contain whitespaces in run-mode. ([#111](https://github.com/peak/s5cmd/pull/111)) [@brendan-matroid](https://github.com/brendan-matroid)


#### Improvements

- Retry when connection closed by S3 unexpectedly. ([#189](https://github.com/peak/s5cmd/pull/189)) [@eminugurkenar](https://github.com/eminugurkenar)

## v1.0.0 - 1 Apr 2020

This is a major release with many breaking changes.

#### Breaking changes

- Dropped `get` command. Users could get the same effect with `s5cmd cp <src> .`.
- Dropped `nested command` support.
- Dropped `!` command. It was used to execute shell commands and was used in
  conjunction with nested commands.
- `s5cmd -f` and `s5cmd -f -` usage has changed to `s5cmd run`. `run` command
  accepts a file. If not provided, it'll listen for commands from stdin.
- Exit code for errors was `127`. It is `1` now.
- Dropped `exit` command. It was used to change the shell exit code and usually
  a part of the nested command usage.
- Dropped local->local copy and move support. ([#118](https://github.com/peak/s5cmd/issues/118))
- All error messages are sent to stderr now.
- `-version` flag is changed to `version` command.
- Dropped `batch-rm` command. It was not listed in the help output. Now that we
  support variadic arguments, users can remove multiple objects by providing
  wildcards or multiple arguments to `s5cmd rm` command. ([#106](https://github.com/peak/s5cmd/pull/106))
- [Virtual host style bucket name
  resolving](https://aws.amazon.com/blogs/aws/amazon-s3-path-deprecation-plan-the-rest-of-the-story/)
  is enabled by default for S3 and GCS. If you provide a custom endpoint via
  `--endpoint-url` flag (other than GCS and S3 transfer acceleration), `s5cmd`
  will fall back to the `path-style`. ([#92](https://github.com/peak/s5cmd/pull/92))
- Listing a non-existent object will return exit code `1`, instead of `0`. ([#23](https://github.com/peak/s5cmd/issues/23))
- `-ds`, `-dw`, `-us` and `-uw` global flags are no longer available. Multipart
  concurrency and part size flags are now part of the `cp/mv` command. New
  replacement flags are `--concurrency | -c` and `--part-size | -p`. ([#110](https://github.com/peak/s5cmd/pull/110))
- s5cmd `cp` command follows symbolic links by default (only when uploading to
  s3 from local filesystem). Use `--no-follow-symlinks` flag to disable this
  feature. ([#17](https://github.com/peak/s5cmd/issues/17))
- Dropped `-parents` flag from copy command. Copy behaviour has changed to
  preserve the directory hierarchy as a default. Optional `-flatten` flag is
  added to flatten directory structure. ([#107](https://github.com/peak/s5cmd/issues/107))
- Dropped `-vv` verbosity flag. `--log` flag is introduced.

#### Features

- Added `mb` command to make buckets. ([#25](https://github.com/peak/s5cmd/issues/25))
- Added `--json` flag for JSON logging. ([#22](https://github.com/peak/s5cmd/issues/22))
- Added [S3 transfer acceleration](https://docs.aws.amazon.com/AmazonS3/latest/dev/transfer-acceleration.html) support. ([#40](https://github.com/peak/s5cmd/issues/40))
- Added [Google Cloud Storage](https://github.com/peak/s5cmd#google-cloud-storage-support) support. ([#81](https://github.com/peak/s5cmd/issues/81))
- Added `cat` command to print remote object contents to stdout ([#20](https://github.com/peak/s5cmd/issues/20))

#### Bugfixes

- Correctly set `Content-Type` of a file on upload operations. ([#33](https://github.com/peak/s5cmd/issues/33))
- Fixed a bug where workers are unable to consume job if there are too many
  outstanding wildcard expansion requests. ([#12](https://github.com/peak/s5cmd/issues/12), [#58](https://github.com/peak/s5cmd/issues/58))

#### Improvements

- Pre-compiled binaries are provided on [releases page](https://github.com/peak/s5cmd/releases) ([#21](https://github.com/peak/s5cmd/issues/21))
- AWS Go SDK is updated to support IAM role for service accounts. ([#32](https://github.com/peak/s5cmd/issues/32))
- For copy/move operations, `s5cmd` now creates destination directory if missing.
- Increase the soft limit of open files to 1000 and exits immediately when it encounters `too many open files` error. ([#52](https://github.com/peak/s5cmd/issues/52))

## v0.7.0 - 27 Jan 2020

- Use go modules.
- Update minimum required Go version to 1.13.

## v0.6.2 - 24 Jan 2020

- Fix bug in brew install.
- Update travis configuration.

## v0.6.1 - 9 Jan 2020

- Integrate Travis CI.
- Add option to disable SSL verification.
- Add endpoint url flag to support S3 compatible services.
- Use client's endpoint in GetSessionForBucket.
- Upgrade minimum required Go version to 1.7.

## v0.6.0 - 30 Mar 2018

- Use 50mb chunks by default.
- Add human-readable output option -H.
- Implement "command -h".

## v0.5.8 - 15 Mar 2018

- Refactor retryable error handling.
- Autodetect bucket region in command completion.
- Add HomeBrew formula.

## v0.5.7 - 16 Aug 2017

- Add -s and -u options to overwrite files if sizes differ or files are lastly modified.
- Use constructor for *JobArgument.

## v0.5.6 - 15 Jun 2017

- Add -dlw, -dlp and -ulw configuration options for worker pool.

## v0.5.5 - 29 May 2017
- Fix get/cp without 2nd param or exact destination filename.

## v0.5.4 - 23 May 2017

- Implement shell auto completion.
- Add context support for batch AWS requests.
- Implement "s5cmd get".
- Reduce idle-timer values.
- Add option -vv to log parser errors verbosely.
- Implement "du -g" to group by storage class.

## v0.5.3 - 9 Mar 2017

- Use Go bool type instead of aws.Bool on recoverer.

## v0.5.2 - 9 Mar 2017

- Make RequestError retryable.

## v0.5.1 - 8 Mar 2017

- Implement verbose output (-vv flag).
- Add godoc for error types.
