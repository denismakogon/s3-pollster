package common

import (
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/sirupsen/logrus"
)

type PreSignedURLs struct {
	GetURL string `json:"get_url"`
	PutURL string `json:"put_url"`
}

type RequestPayload struct {
	Bucket        string        `json:"bucket"`
	Object        string        `json:"object"`
	PreSignedURLs PreSignedURLs `json:"presigned_urls"`
}

func WithDefault(key, defaultValue string) string {
	v := os.Getenv(key)
	if v == "" {
		return defaultValue
	}
	return v
}

func SetupHTTPClient() *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 120 * time.Second,
		}).DialContext,
		MaxIdleConnsPerHost: 512,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig: &tls.Config{
			ClientSessionCache: tls.NewLRUClientSessionCache(4096),
		},
	}
	return &http.Client{Transport: transport}
}

func DoRequest(req *http.Request, httpClient *http.Client, log *logrus.Entry) error {
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode > http.StatusAccepted {
		log.Infof("webhook was submitted successfully with the response: ", string(b))
	} else {
		errMsg := fmt.Errorf("webhook was submitted unsuccessfully with the response: %s", string(b))
		log.Errorf(errMsg.Error())
		return errMsg
	}

	return nil
}
