package command

import (
	"context"
	"fmt"
	"regexp"
	"sync"

	"github.com/hashicorp/go-multierror"
	"github.com/urfave/cli/v2"

	errorpkg "github.com/peak/s5cmd/v2/error"
	"github.com/peak/s5cmd/v2/log"
	"github.com/peak/s5cmd/v2/log/stat"
	"github.com/peak/s5cmd/v2/storage"
	"github.com/peak/s5cmd/v2/storage/url"
)

var deleteHelpTemplate = `Name:
	{{.HelpName}} - {{.Usage}}

Usage:
	{{.HelpName}} argument [argument]

Options:
	{{range .VisibleFlags}}{{.}}
	{{end}}
Examples:
	1. Delete an S3 object
		 > s5cmd {{.HelpName}} s3://bucketname/prefix/object.gz

	2. Delete all objects with a prefix
		 > s5cmd {{.HelpName}} "s3://bucketname/prefix/*"

	3. Delete all objects that matches a wildcard
		 > s5cmd {{.HelpName}} "s3://bucketname/*/obj*.gz"

	4. Delete all matching objects and a specific object
		 > s5cmd {{.HelpName}} "s3://bucketname/prefix/*" s3://bucketname/object1.gz

	5. Delete all matching objects but exclude the ones with .txt extension or starts with "main"
		 > s5cmd {{.HelpName}} --exclude "*.txt" --exclude "main*" "s3://bucketname/prefix/*"

	6. Delete all matching objects but only the ones with .txt extension or starts with "main"
		 > s5cmd {{.HelpName}} --include "*.txt" --include "main*" "s3://bucketname/prefix/*"

	7. Delete the specific version of a remote object's content to stdout
		 > s5cmd {{.HelpName}} --version-id VERSION_ID s3://bucket/prefix/object

	8. Delete all versions of an object in the bucket
		 > s5cmd {{.HelpName}} --all-versions s3://bucket/object

	9. Delete all versions of all objects that starts with a prefix in the bucket
		 > s5cmd {{.HelpName}} --all-versions "s3://bucket/prefix*"

	10. Delete all versions of all objects in the bucket
		 > s5cmd {{.HelpName}} --all-versions "s3://bucket/*"
`

func NewDeleteCommand() *cli.Command {
	cmd := &cli.Command{
		Name:     "rm",
		HelpName: "rm",
		Usage:    "remove objects",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "raw",
				Usage: "disable the wildcard operations, useful with filenames that contains glob characters",
			},
			&cli.StringSliceFlag{
				Name:  "exclude",
				Usage: "exclude objects with given pattern",
			},
			&cli.StringSliceFlag{
				Name:  "include",
				Usage: "include objects with given pattern",
			},
			&cli.BoolFlag{
				Name:  "all-versions",
				Usage: "list all versions of object(s)",
			},
			&cli.StringFlag{
				Name:  "version-id",
				Usage: "use the specified version of an object",
			},
			&cli.StringFlag{
				Name:  "summary-json",
				Usage: "write a JSON summary of the job (counts and bytes deleted and failed, duration, first errors) to the given file when the command ends",
			},
		},
		CustomHelpTemplate: deleteHelpTemplate,
		Before: func(c *cli.Context) error {
			err := validateRMCommand(c)
			if err != nil {
				printError(commandFromContext(c), c.Command.Name, err)
			}
			return err
		},
		Action: func(c *cli.Context) (err error) {
			defer stat.Collect(c.Command.FullName(), &err)()
			defer startSummary(c, &err)()
			fullCommand := commandFromContext(c)

			sources := c.Args().Slice()
			srcUrls, err := newURLs(c.Bool("raw"), c.String("version-id"), c.Bool("all-versions"), sources...)
			if err != nil {
				printError(fullCommand, c.Command.Name, err)
				return err
			}

			excludePatterns, err := createRegexFromWildcard(c.StringSlice("exclude"))
			if err != nil {
				printError(fullCommand, c.Command.Name, err)
				return err
			}

			includePatterns, err := createRegexFromWildcard(c.StringSlice("include"))
			if err != nil {
				printError(fullCommand, c.Command.Name, err)
				return err
			}

			return Delete{
				src:         srcUrls,
				op:          c.Command.Name,
				fullCommand: fullCommand,

				// flags
				exclude: c.StringSlice("exclude"),
				include: c.StringSlice("include"),

				// patterns
				excludePatterns: excludePatterns,
				includePatterns: includePatterns,

				storageOpts: NewStorageOpts(c),
			}.Run(c.Context)
		},
	}

	cmd.BashComplete = getBashCompleteFn(cmd, false, false)
	return cmd
}

// Delete holds delete operation flags and states.
type Delete struct {
	src         []*url.URL
	op          string
	fullCommand string

	// flag options
	exclude []string
	include []string

	// patterns
	excludePatterns []*regexp.Regexp
	includePatterns []*regexp.Regexp

	// storage options
	storageOpts storage.Options
}

// Run remove given sources.
func (d Delete) Run(ctx context.Context) error {

	srcurl := d.src[0]

	client, err := storage.NewClient(ctx, srcurl, d.storageOpts)
	if err != nil {
		printError(d.fullCommand, d.op, err)
		return err
	}

	objch := expandSources(ctx, client, false, d.src...)

	var (
		merrorObjects error
		merrorResult  error
	)

	// sizes of the objects that were sent to be deleted and are not reported
	// yet, for the summary. At most a few chunks of objects are in flight.
	var pendingSizes sync.Map
	trackSizes := activeSummary.Load() != nil

	// do object->url transformation
	urlch := make(chan *url.URL)
	go func() {
		defer close(urlch)

		for object := range objch {
			if (object.Type.IsDir() && !object.DirMarker) || errorpkg.IsCancelation(object.Err) {
				continue
			}

			if err := object.Err; err != nil {
				merrorObjects = multierror.Append(merrorObjects, err)
				printError(d.fullCommand, d.op, err)
				summaryFailed(0, err)
				continue
			}

			isExcluded, err := isObjectExcluded(object, d.excludePatterns, d.includePatterns, srcurl.Prefix)
			if err != nil {
				printError(d.fullCommand, d.op, err)
			}
			if isExcluded {
				continue
			}

			if trackSizes {
				size := object.Size
				if size == 0 {
					size = summaryTakeSizeHint(deleteKey(object.URL))
				}
				println("STORE", deleteKey(object.URL), size)
				pendingSizes.Store(deleteKey(object.URL), size)
			}
			urlch <- object.URL
		}
	}()

	resultch := client.MultiDelete(ctx, urlch)

	for obj := range resultch {
		var size int64
		if trackSizes && obj.URL != nil {
			if v, ok := pendingSizes.LoadAndDelete(deleteKey(obj.URL)); ok {
				size = v.(int64)
			}
		}

		if err := obj.Err; err != nil {
			if errorpkg.IsCancelation(obj.Err) {
				continue
			}

			merrorResult = multierror.Append(merrorResult, obj.Err)
			printError(d.fullCommand, d.op, obj.Err)
			summaryFailed(size, obj.Err)
			continue
		}

		println("RESULT", deleteKey(obj.URL), size)
		summaryDeleted(size)

		msg := log.InfoMessage{
			Operation: d.op,
			Source:    obj.URL,
		}
		log.Info(msg)
	}

	return multierror.Append(merrorResult, merrorObjects).ErrorOrNil()
}

// deleteKey identifies an object in the results of MultiDelete.
func deleteKey(u *url.URL) string {
	return u.Bucket + "/" + u.Path + "?" + u.VersionID
}

// newSources creates object URL list from given sources.
func newURLs(isRaw bool, versionID string, isAllVersions bool, sources ...string) ([]*url.URL, error) {
	var urls []*url.URL
	for _, src := range sources {
		srcurl, err := url.New(src, url.WithRaw(isRaw), url.WithVersion(versionID),
			url.WithAllVersions(isAllVersions))
		if err != nil {
			return nil, err
		}

		if err := checkVersinoningURLRemote(srcurl); err != nil {
			return nil, err
		}
		urls = append(urls, srcurl)
	}
	return urls, nil
}

func validateRMCommand(c *cli.Context) error {
	if !c.Args().Present() {
		return fmt.Errorf("expected at least 1 object to remove")
	}

	// It might be a reasonable request too. Consider that user wants to delete
	// all-versions of "a" and "b", but want to delete only a single
	// version of "c" "someversion". User might want to express this as
	// `s5cmd rm --all-versions a --all-versions b version-id someversion c`
	// but, current implementation does not take repetitive flags into account,
	// anyway, this is not supported in the current implementation.
	if err := checkVersioningFlagCompatibility(c); err != nil {
		return err
	}

	if len(c.Args().Slice()) > 1 && c.String("version-id") != "" {
		return fmt.Errorf("version-id flag can only be used with single source object")
	}

	srcurls, err := newURLs(c.Bool("raw"), c.String("version-id"), c.Bool("all-versions"), c.Args().Slice()...)
	if err != nil {
		return err
	}

	if err := checkVersioningWithGoogleEndpoint(c); err != nil {
		return err
	}

	var (
		firstBucket         string
		hasRemote, hasLocal bool
	)
	for i, srcurl := range srcurls {
		// we don't operate on S3 prefixes for copy and delete operations.
		// With --raw the user names an object key that happens to end with
		// "/", so it is not treated as a prefix.
		if srcurl.IsBucket() || (srcurl.IsPrefix() && !srcurl.IsRaw()) {
			return fmt.Errorf("s3 bucket/prefix cannot be used for delete operations (forgot wildcard character?)")
		}

		// Listing all versions of a key works by listing the key as a prefix,
		// which for "dir/" would also match every key below it.
		if srcurl.IsPrefix() && srcurl.AllVersions {
			return fmt.Errorf("--raw with --all-versions cannot be used with a key ending in \"/\", it would match every key under that prefix")
		}

		if srcurl.IsRemote() {
			hasRemote = true
		} else {
			hasLocal = true
		}

		if hasLocal && hasRemote {
			return fmt.Errorf("arguments cannot have both local and remote sources")
		}
		if i == 0 {
			firstBucket = srcurl.Bucket
			continue
		}
		if srcurl.Bucket != firstBucket {
			return fmt.Errorf("removal of objects with different buckets in a single command is not allowed")
		}
	}

	return nil
}
