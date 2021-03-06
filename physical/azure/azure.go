package azure

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-storage-blob-go/azblob"
	"github.com/Azure/go-autorest/autorest/azure"
	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/errwrap"
	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/vault/sdk/helper/strutil"
	"github.com/hashicorp/vault/sdk/physical"
)

const (
	// MaxBlobSize at this time
	MaxBlobSize = 1024 * 1024 * 4
	// MaxListResults is the current default value, setting explicitly
	MaxListResults = 5000
)

// AzureBackend is a physical backend that stores data
// within an Azure blob container.
type AzureBackend struct {
	container  *azblob.ContainerURL
	logger     log.Logger
	permitPool *physical.PermitPool
}

// Verify AzureBackend satisfies the correct interfaces
var _ physical.Backend = (*AzureBackend)(nil)

// NewAzureBackend constructs an Azure backend using a pre-existing
// bucket. Credentials can be provided to the backend, sourced
// from the environment, AWS credential files or by IAM role.
func NewAzureBackend(conf map[string]string, logger log.Logger) (physical.Backend, error) {
	name := os.Getenv("AZURE_BLOB_CONTAINER")
	if name == "" {
		name = conf["container"]
		if name == "" {
			return nil, fmt.Errorf("'container' must be set")
		}
	}

	accountName := os.Getenv("AZURE_ACCOUNT_NAME")
	if accountName == "" {
		accountName = conf["accountName"]
		if accountName == "" {
			return nil, fmt.Errorf("'accountName' must be set")
		}
	}

	accountKey := os.Getenv("AZURE_ACCOUNT_KEY")
	if accountKey == "" {
		accountKey = conf["accountKey"]
		if accountKey == "" {
			return nil, fmt.Errorf("'accountKey' must be set")
		}
	}

	environmentName := os.Getenv("AZURE_ENVIRONMENT")
	if environmentName == "" {
		environmentName = conf["environment"]
		if environmentName == "" {
			environmentName = "AzurePublicCloud"
		}
	}

	environmentURL := os.Getenv("AZURE_ARM_ENDPOINT")
	if environmentURL == "" {
		environmentURL = conf["arm_endpoint"]
	}

	var environment azure.Environment
	var err error

	if environmentURL != "" {
		environment, err = azure.EnvironmentFromURL(environmentURL)
		if err != nil {
			errorMsg := fmt.Sprintf("failed to look up Azure environment descriptor for URL %q: {{err}}",
				environmentURL)
			return nil, errwrap.Wrapf(errorMsg, err)
		}
	} else {
		environment, err = azure.EnvironmentFromName(environmentName)
		if err != nil {
			errorMsg := fmt.Sprintf("failed to look up Azure environment descriptor for name %q: {{err}}",
				environmentName)
			return nil, errwrap.Wrapf(errorMsg, err)
		}
	}

	credential, err := azblob.NewSharedKeyCredential(accountName, accountKey)
	if err != nil {
		return nil, errwrap.Wrapf("failed to create Azure client: {{err}}", err)
	}

	URL, err := url.Parse(
		fmt.Sprintf("https://%s.blob.%s/%s", accountName, environment.StorageEndpointSuffix, name))
	if err != nil {
		return nil, errwrap.Wrapf("failed to create Azure client: {{err}}", err)
	}

	p := azblob.NewPipeline(credential, azblob.PipelineOptions{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	containerURL := azblob.NewContainerURL(*URL, p)
	_, err = containerURL.GetProperties(ctx, azblob.LeaseAccessConditions{})
	if err != nil {
		var e azblob.StorageError
		if errors.As(err, &e) {
			switch e.ServiceCode() {
			case azblob.ServiceCodeContainerNotFound:
				_, err := containerURL.Create(ctx, azblob.Metadata{}, azblob.PublicAccessNone)
				if err != nil {
					return nil, errwrap.Wrapf(fmt.Sprintf("failed to create %q container: {{err}}", name), err)
				}
			default:
				return nil, errwrap.Wrapf(fmt.Sprintf("failed to get properties for container %q: {{err}}", name), err)
			}
		}
	}

	maxParStr, ok := conf["max_parallel"]
	var maxParInt int
	if ok {
		maxParInt, err = strconv.Atoi(maxParStr)
		if err != nil {
			return nil, errwrap.Wrapf("failed parsing max_parallel parameter: {{err}}", err)
		}
		if logger.IsDebug() {
			logger.Debug("max_parallel set", "max_parallel", maxParInt)
		}
	}

	a := &AzureBackend{
		container:  &containerURL,
		logger:     logger,
		permitPool: physical.NewPermitPool(maxParInt),
	}
	return a, nil
}

// Put is used to insert or update an entry
func (a *AzureBackend) Put(ctx context.Context, entry *physical.Entry) error {
	defer metrics.MeasureSince([]string{"azure", "put"}, time.Now())

	if len(entry.Value) >= MaxBlobSize {
		return fmt.Errorf("value is bigger than the current supported limit of 4MBytes")
	}

	a.permitPool.Acquire()
	defer a.permitPool.Release()

	blobURL := a.container.NewBlockBlobURL(entry.Key)
	_, err := azblob.UploadBufferToBlockBlob(ctx, entry.Value, blobURL, azblob.UploadToBlockBlobOptions{
		BlockSize: MaxBlobSize,
	})

	return err
}

// Get is used to fetch an entry
func (a *AzureBackend) Get(ctx context.Context, key string) (*physical.Entry, error) {
	defer metrics.MeasureSince([]string{"azure", "get"}, time.Now())

	a.permitPool.Acquire()
	defer a.permitPool.Release()

	blobURL := a.container.NewBlockBlobURL(key)
	res, err := blobURL.Download(ctx, 0, 0, azblob.BlobAccessConditions{}, false)
	if err != nil {
		var e azblob.StorageError
		if errors.As(err, &e) {
			switch e.ServiceCode() {
			case azblob.ServiceCodeBlobNotFound:
				return nil, nil
			default:
				return nil, errwrap.Wrapf(fmt.Sprintf("failed to download blob %q: {{err}}", key), err)
			}
		}
		return nil, err
	}

	reader := res.Body(azblob.RetryReaderOptions{})

	defer reader.Close()
	data, err := ioutil.ReadAll(reader)

	ent := &physical.Entry{
		Key:   key,
		Value: data,
	}

	return ent, err
}

// Delete is used to permanently delete an entry
func (a *AzureBackend) Delete(ctx context.Context, key string) error {
	defer metrics.MeasureSince([]string{"azure", "delete"}, time.Now())

	a.permitPool.Acquire()
	defer a.permitPool.Release()

	blobURL := a.container.NewBlockBlobURL(key)
	_, err := blobURL.Delete(ctx, azblob.DeleteSnapshotsOptionInclude, azblob.BlobAccessConditions{})
	if err != nil {
		var e azblob.StorageError
		if errors.As(err, &e) {
			switch e.ServiceCode() {
			case azblob.ServiceCodeBlobNotFound:
				return nil
			default:
				return errwrap.Wrapf(fmt.Sprintf("failed to delete blob %q: {{err}}", key), err)
			}
		}
	}

	return err
}

// List is used to list all the keys under a given
// prefix, up to the next prefix.
func (a *AzureBackend) List(ctx context.Context, prefix string) ([]string, error) {
	defer metrics.MeasureSince([]string{"azure", "list"}, time.Now())

	a.permitPool.Acquire()
	defer a.permitPool.Release()

	keys := []string{}
	for marker := (azblob.Marker{}); marker.NotDone(); {
		listBlob, err := a.container.ListBlobsFlatSegment(ctx, marker, azblob.ListBlobsSegmentOptions{
			Prefix:     prefix,
			MaxResults: MaxListResults,
		})
		if err != nil {
			return nil, err
		}

		for _, blobInfo := range listBlob.Segment.BlobItems {
			key := strings.TrimPrefix(blobInfo.Name, prefix)
			if i := strings.Index(key, "/"); i == -1 {
				// file
				keys = append(keys, key)
			} else {
				// subdirectory
				keys = strutil.AppendIfMissing(keys, key[:i+1])
			}
		}

		marker = listBlob.NextMarker
	}

	sort.Strings(keys)
	return keys, nil
}
