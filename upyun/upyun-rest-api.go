package upyun

import (
	"bytes"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	URL "net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

// UPYUN REST API Client
type UpYun struct {
	// Core
	upYunHTTPCore

	Bucket    string
	Username  string
	Passwd    string
	ChunkSize int
}

// NewUpYun return a new UPYUN REST API client given a bucket name,
// username, password. As Default, endpoint is set to Auto, http
// client connection timeout is set to defalutConnectionTimeout which
// is equal to 60 seconds.
func NewUpYun(bucket, username, passwd string) *UpYun {
	u := &UpYun{
		Bucket:   bucket,
		Username: username,
		Passwd:   passwd,
	}

	u.httpClient = &http.Client{}
	u.SetEndpoint(Auto)
	u.SetTimeout(defaultConnectTimeout)

	return u
}

// SetEndpoint sets the request endpoint to UPYUN REST API Server.
func (u *UpYun) SetEndpoint(ed int) error {
	if ed >= Auto && ed <= Ctt {
		u.endpoint = fmt.Sprintf("v%d.api.upyun.com", ed)
		return nil
	}

	return errors.New("Invalid endpoint, pick from Auto, Telecom, Cnc, Ctt")
}

// SetEndpointStr sets the request endpoint to UPYUN REST API Server.
func (u *UpYun) SetEndpointStr(endpoint string) error {
	u.endpoint = endpoint
	return nil
}

// make UpYun REST Authorization
func (u *UpYun) makeRESTAuth(method, uri, date, lengthStr string) string {
	sign := []string{method, uri, date, lengthStr, md5Str(u.Passwd)}

	return "UpYun " + u.Username + ":" + md5Str(strings.Join(sign, "&"))
}

// make UpYun Purge Authorization
func (u *UpYun) makePurgeAuth(purgeList, date string) string {
	sign := []string{purgeList, u.Bucket, date, md5Str(u.Passwd)}

	return "UpYun " + u.Bucket + ":" + u.Username + ":" + md5Str(strings.Join(sign, "&"))
}

// Usage gets the usage of the bucket in UPYUN File System
func (u *UpYun) Usage() (int64, error) {
	result, _, err := u.doRESTRequest("GET", "/", "usage", nil, nil)
	if err != nil {
		return 0, err
	}

	return strconv.ParseInt(result, 10, 64)
}

// Mkdir creates a directory in UPYUN File System
func (u *UpYun) Mkdir(key string) error {
	headers := make(map[string]string)

	headers["mkdir"] = "true"
	headers["folder"] = "true"

	_, _, err := u.doRESTRequest("POST", key, "", headers, nil)

	return err
}

// Put uploads filelike object to UPYUN File System
func (u *UpYun) Put(key string, value io.Reader, useMD5 bool,
	headers map[string]string) (http.Header, error) {
	if headers == nil {
		headers = make(map[string]string)
	}

	if _, ok := headers["Content-Length"]; !ok {
		switch v := value.(type) {
		case *os.File:
			if fileInfo, err := v.Stat(); err != nil {
				return nil, err
			} else {
				headers["Content-Length"] = fmt.Sprint(fileInfo.Size())
			}
		default:
			// max buffer is 10k
			rw := bytes.NewBuffer(make([]byte, 0, 10240))
			if n, err := io.Copy(rw, value); err != nil {
				return nil, err
			} else {
				headers["Content-Length"] = fmt.Sprint(n)
			}
			value = rw
		}
	}

	if _, ok := headers["Content-MD5"]; !ok && useMD5 {
		switch v := value.(type) {
		case *os.File:
			hash := md5.New()
			if _, err := io.Copy(hash, value); err != nil {
				return nil, err
			}

			headers["Content-MD5"] = fmt.Sprintf("%x", hash.Sum(nil))

			if _, err := v.Seek(0, 0); err != nil {
				return nil, err
			}
		}
	}

	_, rtHeaders, err := u.doRESTRequest("PUT", key, "", headers, value)

	return rtHeaders, err
}

// Put uploads file object to UPYUN File System part by part,
// and automatically retries when a network problem occurs
func (u *UpYun) ResumePut(key string, value *os.File, useMD5 bool,
	headers map[string]string, reporter ResumeReporter) (http.Header, error) {
	if headers == nil {
		headers = make(map[string]string)
	}

	fileinfo, err := value.Stat()
	if err != nil {
		return nil, err
	}

	// If filesize < resumePartSizeLowerLimit, use UpYun.Put() instead
	if fileinfo.Size() < resumeFileSizeLowerLimit {
		return u.Put(key, value, useMD5, headers)
	}

	maxPartID := int(fileinfo.Size() / resumePartSize)
	if fileinfo.Size()%resumePartSize == 0 {
		maxPartID--
	}

	var resp http.Header

	for part := 0; part <= maxPartID; part++ {

		innerHeaders := make(map[string]string)
		for k, v := range headers {
			innerHeaders[k] = v
		}

		innerHeaders["X-Upyun-Part-Id"] = strconv.Itoa(part)
		switch part {
		case 0:
			innerHeaders["X-Upyun-Multi-Type"] = headers["Content-Type"]
			innerHeaders["X-Upyun-Multi-Length"] = strconv.FormatInt(fileinfo.Size(), 10)
			innerHeaders["X-Upyun-Multi-Stage"] = "initiate,upload"
			innerHeaders["Content-Length"] = strconv.Itoa(resumePartSize)
		case maxPartID:
			innerHeaders["X-Upyun-Multi-Stage"] = "upload,complete"
			innerHeaders["Content-Length"] = fmt.Sprint(fileinfo.Size() - int64(resumePartSize)*int64(part))
			if useMD5 {
				value.Seek(0, 0)
				hex, _, _ := md5sum(value)
				innerHeaders["X-Upyun-Multi-MD5"] = hex
			}
		default:
			innerHeaders["X-Upyun-Multi-Stage"] = "upload"
			innerHeaders["Content-Length"] = strconv.Itoa(resumePartSize)
		}

		file, err := NewFragmentFile(value, int64(part)*int64(resumePartSize), resumePartSize)
		if err != nil {
			return resp, err
		}
		if useMD5 {
			innerHeaders["Content-MD5"], _ = file.MD5()
		}

		// Retry when get net error from UpYun.Put(), return error in other cases
		for i := 0; i < ResumeRetryCount+1; i++ {
			resp, err = u.Put(key, file, useMD5, innerHeaders)
			if err == nil {
				break
			}
			// Retry only get net error
			_, ok := err.(net.Error)
			if !ok {
				return resp, err
			}
			if i == ResumeRetryCount {
				return resp, err
			}
			time.Sleep(ResumeWaitTime)
			file.Seek(0, 0)
		}
		if reporter != nil {
			reporter(part, maxPartID)
		}

		if part == 0 {
			headers["X-Upyun-Multi-UUID"] = resp.Get("X-Upyun-Multi-Uuid")
		}
	}

	return resp, nil
}

// Get gets the specified file in UPYUN File System
func (u *UpYun) Get(key string, value io.Writer) (int, error) {
	length, _, err := u.doRESTRequest("GET", key, "", nil, value)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(length)
}

// Delete deletes the specified **file** in UPYUN File System.
func (u *UpYun) Delete(key string) error {
	_, _, err := u.doRESTRequest("DELETE", key, "", nil, nil)

	return err
}

// AsyncDelete deletes the specified **file** in UPYUN File System asynchronously.
func (u *UpYun) AsyncDelete(key string) error {
	headers := map[string]string{
		"X-Upyun-Async": "true",
	}
	_, _, err := u.doRESTRequest("DELETE", key, "", headers, nil)

	return err
}

// GetList lists items in key. The number of items must be
// less then 100
func (u *UpYun) GetList(key string) ([]*FileInfo, error) {
	ret, _, err := u.doRESTRequest("GET", key, "", nil, nil)
	if err != nil {
		return nil, err
	}

	list := strings.Split(ret, "\n")
	var infoList []*FileInfo

	for _, v := range list {
		if len(v) == 0 {
			continue
		}
		infoList = append(infoList, newFileInfo(v))
	}

	return infoList, nil
}

// Note: key must be directory
func (u *UpYun) GetLargeList(key string, asc, recursive bool) (chan *FileInfo, chan error) {
	infoChannel := make(chan *FileInfo, 1000)
	errChannel := make(chan error, 10)
	if !strings.HasSuffix(key, "/") {
		key += "/"
	}
	order := "desc"
	if asc == true {
		order = "asc"
	}

	go func() {
		var listDir func(k string) error
		listDir = func(k string) error {
			var infos []*FileInfo
			var niter string
			var err error
			iter, limit := "", 50
			for {
				infos, niter, err = u.loopList(k, iter, order, limit)
				if err != nil {
					errChannel <- err
					return err
				}
				iter = niter
				for _, f := range infos {
					// absolute path
					abs := path.Join(k, f.Name)
					// relative path
					f.Name = strings.Replace(abs, key, "", 1)
					if f.Name[0] == '/' {
						f.Name = f.Name[1:]
					}
					if recursive && f.Type == "folder" {
						if err = listDir(abs + "/"); err != nil {
							return err
						}
					}
					infoChannel <- f
				}
				if iter == "" {
					break
				}
			}
			return nil
		}

		listDir(key)

		close(errChannel)
		close(infoChannel)
	}()

	return infoChannel, errChannel
}

// LoopList list items iteratively.
func (u *UpYun) loopList(key, iter, order string, limit int) ([]*FileInfo, string, error) {
	headers := map[string]string{
		"X-List-Limit": fmt.Sprint(limit),
		"X-List-Order": order,
	}
	if iter != "" {
		headers["X-List-Iter"] = iter
	}

	ret, rtHeaders, err := u.doRESTRequest("GET", key, "", headers, nil)
	if err != nil {
		return nil, "", err
	}

	list := strings.Split(ret, "\n")
	var infoList []*FileInfo
	for _, v := range list {
		if len(v) == 0 {
			continue
		}
		infoList = append(infoList, newFileInfo(v))
	}

	nextIter := ""
	if _, ok := rtHeaders["X-Upyun-List-Iter"]; ok {
		nextIter = rtHeaders["X-Upyun-List-Iter"][0]
	} else {
		// Maybe Wrong
		return nil, "", nil
	}

	if nextIter == "g2gCZAAEbmV4dGQAA2VvZg" {
		nextIter = ""
	}

	return infoList, nextIter, nil
}

// GetInfo gets information of item in UPYUN File System
func (u *UpYun) GetInfo(key string) (*FileInfo, error) {
	_, headers, err := u.doRESTRequest("HEAD", key, "", nil, nil)
	if err != nil {
		return nil, err
	}

	fileInfo := newFileInfo(headers)

	return fileInfo, nil
}

// Purge post a purge request to UPYUN Purge Server
func (u *UpYun) Purge(urls []string) (string, error) {
	purge := "http://purge.upyun.com/purge/"

	date := genRFC1123Date()
	purgeList := strings.Join(urls, "\n")

	headers := make(map[string]string)
	headers["Date"] = date
	headers["Authorization"] = u.makePurgeAuth(purgeList, date)
	headers["Content-Type"] = "application/x-www-form-urlencoded;charset=utf-8"

	form := make(URL.Values)
	form.Add("purge", purgeList)

	body := strings.NewReader(form.Encode())
	resp, err := u.doHTTPRequest("POST", purge, headers, body)
	defer resp.Body.Close()

	content, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode/100 == 2 {
		result := make(map[string][]string)
		if err := json.Unmarshal(content, &result); err != nil {
			// quick fix for invalid json resp: {"invalid_domain_of_url":{}}
			return "", nil
		}

		return strings.Join(result["invalid_domain_of_url"], ","), nil
	}

	return "", errors.New(string(content))
}

func (u *UpYun) doRESTRequest(method, uri, query string, headers map[string]string,
	value interface{}) (result string, rtHeaders http.Header, err error) {
	if headers == nil {
		headers = make(map[string]string)
	}

	// Normalize url
	if !strings.HasPrefix(uri, "/") {
		uri = "/" + uri
	}

	uri = escapeURI("/" + u.Bucket + uri)
	url := fmt.Sprintf("http://%s%s", u.endpoint, uri)

	if query != "" {
		query = escapeURI(query)
		url += "?" + query
	}

	// date
	date := genRFC1123Date()

	// auth
	lengthStr, ok := headers["Content-Length"]
	if !ok {
		lengthStr = "0"
	}

	headers["Date"] = date
	headers["Authorization"] = u.makeRESTAuth(method, uri, date, lengthStr)
	if !strings.Contains(u.endpoint, "api.upyun.com") {
		headers["Host"] = "v0.api.upyun.com"
	}

	// HEAD GET request has no body
	rc, ok := value.(io.Reader)
	if !ok || method == "GET" || method == "HEAD" {
		rc = nil
	}

	resp, err := u.doHTTPRequest(method, url, headers, rc)
	if err != nil {
		return "", nil, err
	}

	defer resp.Body.Close()

	if (resp.StatusCode / 100) == 2 {
		if method == "GET" && value != nil {
			written, err := chunkedCopy(value.(io.Writer), resp.Body)
			return strconv.FormatInt(written, 10), resp.Header, err
		}
		body, err := ioutil.ReadAll(resp.Body)
		return string(body), resp.Header, err
	}

	if body, err := ioutil.ReadAll(resp.Body); err == nil {
		if len(body) == 0 && resp.StatusCode/100 != 2 {
			return "", resp.Header, errors.New(fmt.Sprint(resp.StatusCode))
		}
		return "", resp.Header, errors.New(string(body))
	} else {
		return "", resp.Header, err
	}
}
