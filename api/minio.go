package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/denismakogon/go-structs"
	"github.com/denismakogon/s3-pollster/common"
	"github.com/sirupsen/logrus"
)

type store struct {
	client     *s3.S3
	uploader   *s3manager.Uploader
	downloader *s3manager.Downloader
	bucket     string
	config     *MinioConfig
}

func (m *MinioConfig) createStore() *store {
	client := s3.New(session.Must(session.NewSession(&aws.Config{
		Credentials:      credentials.NewStaticCredentials(m.AccessKeyID, m.SecretAccessKey, ""),
		Endpoint:         aws.String(m.Endpoint),
		Region:           aws.String(m.Region),
		DisableSSL:       aws.Bool(!m.UseSSL),
		S3ForcePathStyle: aws.Bool(true),
	})))
	return &store{
		client:     client,
		config:     m,
		uploader:   s3manager.NewUploaderWithClient(client),
		downloader: s3manager.NewDownloaderWithClient(client),
	}
}

type MinioConfig struct {
	Bucket          string `json:"bucket"`
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	UseSSL          bool   `json:"use_ssl"`
}

func (m *MinioConfig) FromURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return err
	}

	endpoint := u.Host

	var accessKeyID, secretAccessKey string
	if u.User != nil {
		accessKeyID = u.User.Username()
		secretAccessKey, _ = u.User.Password()
	}
	useSSL := u.Query().Get("ssl") == "true"

	strs := strings.SplitN(u.Path, "/", 3)
	if len(strs) < 3 {
		return errors.New("must provide bucket name and region in path of s3 api url. e.g. s3://s3.com/us-east-1/my_bucket")
	}
	region := strs[1]
	bucketName := strs[2]
	if region == "" {
		return errors.New("must provide non-empty region in path of s3 api url. e.g. s3://s3.com/us-east-1/my_bucket")
	} else if bucketName == "" {
		return errors.New("must provide non-empty bucket name in path of s3 api url. e.g. s3://s3.com/us-east-1/my_bucket")
	}

	m.Bucket = bucketName
	m.Endpoint = endpoint
	m.Region = region
	m.AccessKeyID = accessKeyID
	m.SecretAccessKey = secretAccessKey
	m.UseSSL = useSSL

	return nil
}

func (m *MinioConfig) ToMap() (map[string]interface{}, error) {
	return structs.ToMap(m)
}

func New() (*store, error) {
	m := &MinioConfig{}

	mURL := common.WithDefault("S3_URL",
		"s3://admin:password@s3:9000/us-east-1/default-bucket")
	logrus.Println("S3 URL: ", mURL)

	err := m.FromURL(mURL)
	if err != nil {
		return nil, err
	}
	logFields, err := m.ToMap()
	if err != nil {
		return nil, err
	}

	logrus.WithFields(logFields).Info("checking / creating s3 bucket")

	store := m.createStore()

	_, err = store.client.CreateBucket(&s3.CreateBucketInput{Bucket: aws.String(m.Bucket)})
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case s3.ErrCodeBucketAlreadyOwnedByYou, s3.ErrCodeBucketAlreadyExists:
				// bucket already exists, NO-OP
			default:
				return nil, fmt.Errorf("failed to create bucket %s: %s", m.Bucket, aerr.Message())
			}
		} else {
			return nil, fmt.Errorf("unexpected error creating bucket %s: %s", m.Bucket, err.Error())
		}
	}

	return store, nil
}

func (s *store) asyncDispatcher(ctx context.Context, wg sync.WaitGroup, log *logrus.Entry, input *s3.ListObjectsInput,
	req *http.Request, httpClient *http.Client) error {

	result, err := s.client.ListObjectsWithContext(ctx, input)
	if err != nil {
		return err
	}
	fields := logrus.Fields{}
	fields["current_key"] = *result.Marker
	fields["objects_found"] = len(result.Contents)
	if result.NextMarker != nil {
		fields["next_query_key"] = *result.NextMarker
	}
	log = log.WithFields(fields)
	var b bytes.Buffer
	if len(result.Contents) > 0 {
		wg.Add(len(result.Contents))
		for _, object := range result.Contents {

			go func(wg sync.WaitGroup, object *s3.Object) {
				defer wg.Done()

				err := func() error {
					log.Info("Sending the object: ", s.config.Bucket+"/"+*object.Key)
					getR, _ := s.client.GetObjectRequest(&s3.GetObjectInput{
						Bucket: aws.String(s.config.Bucket),
						Key:    object.Key,
					})
					getRstr, err := getR.Presign(1 * time.Hour)
					if err != nil {
						return err
					}

					putR, _ := s.client.PutObjectRequest(&s3.PutObjectInput{
						Bucket: aws.String(s.config.Bucket),
						Key:    object.Key,
					})
					putRstr, err := putR.Presign(1 * time.Hour)
					if err != nil {
						return err
					}

					payload := &common.RequestPayload{
						Bucket: s.config.Bucket,
						Object: *object.Key,
						PreSignedURLs: common.PreSignedURLs{
							GetURL: getRstr,
							PutURL: putRstr,
						},
					}
					b.Reset()
					err = json.NewEncoder(&b).Encode(&payload)
					if err != nil {
						return err
					}

					req.Body = ioutil.NopCloser(&b)
					err = common.DoRequest(req, httpClient, log)
					if err != nil {
						return err
					}

					return nil
				}()
				if err != nil {
					log.Error(err.Error())
				}

			}(wg, object)
		}
		input.SetMarker(*result.NextMarker)
	}

	return nil
}

func (s *store) DispatchObjects(ctx context.Context, wg sync.WaitGroup) error {
	log := logrus.WithFields(logrus.Fields{"bucketName": s.config.Bucket})

	input := &s3.ListObjectsInput{
		Bucket:  aws.String(s.config.Bucket),
		MaxKeys: aws.Int64(10),
		Marker:  aws.String(""),
	}
	webkookEndpoint := os.Getenv("WEBHOOK_ENDPOINT")
	if webkookEndpoint == "" {
		return errors.New("WEBHOOK_ENDPOINT is not set")
	}

	_, err := url.Parse(webkookEndpoint)
	if err != nil {
		return fmt.Errorf("invalid webook URL: %s", err.Error())
	}

	req, err := http.NewRequest(http.MethodPost, webkookEndpoint, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	httpClient := common.SetupHTTPClient()

	backoff := common.WithDefault("POLLSTER_BACKOFF", "5")
	intBackoff, _ := strconv.Atoi(backoff)

	for {

		err = s.asyncDispatcher(ctx, wg, log, input, req, httpClient)
		if err != nil {
			return err
		}

		time.Sleep(time.Duration(intBackoff) * time.Second)
	}

	wg.Wait()

	return nil
}
