package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FrankerFaceZ/FrankerFaceZ/socketserver/server/naclform"
	cache "github.com/patrickmn/go-cache"
	"golang.org/x/crypto/nacl/box"
	"golang.org/x/sync/singleflight"
)

const bPathAnnounceStartup = "/startup"
const bPathAddTopic = "/topics"
const bPathAggStats = "/stats"
const bPathOtherCommand = "/cmd/"

type backendInfo struct {
	HTTPClient    http.Client
	baseURL       string
	responseCache *cache.Cache
	reloadGroup   singleflight.Group

	postStatisticsURL  string
	addTopicURL        string
	announceStartupURL string

	secureForm naclform.ServerInfo

	lastSuccess     map[string]time.Time
	lastSuccessLock sync.Mutex
}

var Backend *backendInfo

func setupBackend(config *ConfigFile) *backendInfo {
	b := new(backendInfo)
	Backend = b
	b.secureForm.ServerID = config.ServerID

	b.HTTPClient.Timeout = 60 * time.Second
	b.baseURL = config.BackendURL
	// size in bytes of string payload
	b.responseCache = cache.New(60*time.Second, 10*time.Minute)

	b.announceStartupURL = fmt.Sprintf("%s%s", b.baseURL, bPathAnnounceStartup)
	b.addTopicURL = fmt.Sprintf("%s%s", b.baseURL, bPathAddTopic)
	b.postStatisticsURL = fmt.Sprintf("%s%s", b.baseURL, bPathAggStats)

	epochTime := time.Unix(0, 0).UTC()
	lastBackendSuccess := map[string]time.Time{
		bPathAnnounceStartup: epochTime,
		bPathAddTopic:        epochTime,
		bPathAggStats:        epochTime,
		bPathOtherCommand:    epochTime,
	}
	b.lastSuccess = lastBackendSuccess

	var theirPublic, ourPrivate [32]byte
	copy(theirPublic[:], config.BackendPublicKey)
	copy(ourPrivate[:], config.OurPrivateKey)

	box.Precompute(&b.secureForm.SharedKey, &theirPublic, &ourPrivate)

	return b
}

func getCacheKey(remoteCommand, data string) string {
	return fmt.Sprintf("%s/%s", remoteCommand, data)
}

// ErrForwardedFromBackend is an error returned by the backend server.
type ErrForwardedFromBackend struct {
	JSONError interface{}
}

func (bfe ErrForwardedFromBackend) Error() string {
	bytes, _ := json.Marshal(bfe.JSONError)
	return string(bytes)
}

// ErrAuthorizationNeeded is emitted when the backend replies with HTTP 401.
//
// Indicates that an attempt to validate `ClientInfo.TwitchUsername` should be attempted.
var ErrAuthorizationNeeded = errors.New("Must authenticate Twitch username to use this command")

// SendRemoteCommandCached performs a RPC call on the backend, checking for a
// cached response first.
//
// If a cached, but expired, response is found, the existing value is returned
// and the cache is updated in the background.
func (backend *backendInfo) SendRemoteCommandCached(remoteCommand, data string, auth AuthInfo) (string, error) {
	cacheKey := getCacheKey(remoteCommand, data)
	cached, ok := backend.responseCache.Get(cacheKey)
	if ok {
		return cached.(string), nil
	}
	return backend.SendRemoteCommand(remoteCommand, data, auth)
}

// SendRemoteCommand performs a RPC call on the backend by POSTing to `/cmd/$remoteCommand`.
//
// The form data is as follows: `clientData` is the JSON in the `data` parameter
// (should be retrieved from ClientMessage.Arguments), `username` is AuthInfo.TwitchUsername,
// and `authenticated` is 1 or 0 depending on AuthInfo.UsernameValidated.
//
// 401 responses return an ErrAuthorizationNeeded.
//
// Non-2xx responses return the response body as an error to the client (application/json
// responses are sent as-is, non-json are sent as a JSON string).
//
// If a 2xx response has the FFZ-Cache header, its value is used as a minimum number of
// seconds to cache the response for. (Responses may be cached for longer, see
// SendRemoteCommandCached and the cache implementation.)
//
// A successful response updates the Statistics.Health.Backend map.
func (backend *backendInfo) SendRemoteCommand(remoteCommand, data string, auth AuthInfo) (responseStr string, err error) {
	destURL := fmt.Sprintf("%s/cmd/%s", backend.baseURL, remoteCommand)
	healthBucket := fmt.Sprintf("/cmd/%s", remoteCommand)

	formData := url.Values{
		"clientData": []string{data},
		"username":   []string{auth.TwitchUsername},
	}

	if auth.UsernameValidated {
		formData.Set("authenticated", "1")
	} else {
		formData.Set("authenticated", "0")
	}

	sealedForm, err := backend.secureForm.Seal(formData)
	if err != nil {
		return "", err
	}

	resp, err := backend.HTTPClient.PostForm(destURL, sealedForm)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	responseStr = string(respBytes)

	if resp.StatusCode == 401 {
		return "", ErrAuthorizationNeeded
	} else if resp.StatusCode < 200 || resp.StatusCode > 299 { // any non-2xx
		// If the Content-Type header includes a charset, ignore it.
		// typeStr, _, _ = mime.ParseMediaType(resp.Header.Get("Content-Type"))
		// inline the part of the function we care about
		typeStr := resp.Header.Get("Content-Type")
		splitIdx := strings.IndexRune(typeStr, ';')
		if splitIdx != -1 {
			typeStr = strings.TrimSpace(typeStr[0:splitIdx])
		}

		if typeStr == "application/json" {
			var err2 ErrForwardedFromBackend
			err := json.Unmarshal(respBytes, &err2.JSONError)
			if err != nil {
				return "", fmt.Errorf("error decoding json error from backend: %v | %s", err, responseStr)
			}
			return "", err2
		}
		return "", httpError(resp.StatusCode)
	}

	if resp.Header.Get("FFZ-Cache") != "" {
		durSecs, err := strconv.ParseInt(resp.Header.Get("FFZ-Cache"), 10, 64)
		if err != nil {
			return "", fmt.Errorf("The RPC server returned a non-integer cache duration: %v", err)
		}
		duration := time.Duration(durSecs) * time.Second
		backend.responseCache.Set(
			getCacheKey(remoteCommand, data),
			responseStr,
			duration,
		)
	}

	now := time.Now().UTC()
	backend.lastSuccessLock.Lock()
	defer backend.lastSuccessLock.Unlock()
	backend.lastSuccess[bPathOtherCommand] = now
	backend.lastSuccess[healthBucket] = now

	return
}

// SendAggregatedData sends aggregated emote usage and following data to the backend server.
func (backend *backendInfo) SendAggregatedData(form url.Values) error {
	sealedForm, err := backend.secureForm.Seal(form)
	if err != nil {
		return err
	}

	resp, err := backend.HTTPClient.PostForm(backend.postStatisticsURL, sealedForm)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		resp.Body.Close()
		return httpError(resp.StatusCode)
	}

	backend.lastSuccessLock.Lock()
	defer backend.lastSuccessLock.Unlock()
	backend.lastSuccess[bPathAggStats] = time.Now().UTC()

	return resp.Body.Close()
}

// ErrBackendNotOK indicates that the backend replied with something other than the string "ok".
type ErrBackendNotOK struct {
	Response string
	Code     int
}

// Error Implements the error interface.
func (noe ErrBackendNotOK) Error() string {
	return fmt.Sprintf("backend returned %d: %s", noe.Code, noe.Response)
}

// SendNewTopicNotice notifies the backend that a client has performed the first subscription to a pub/sub topic.
// POST data:
// channels=room.trihex
// added=t
func (backend *backendInfo) SendNewTopicNotice(topic string) error {
	return backend.sendTopicNotice(topic, true)
}

// SendCleanupTopicsNotice notifies the backend that pub/sub topics have no subscribers anymore.
// POST data:
// channels=room.sirstendec,room.bobross,feature.foo
// added=f
func (backend *backendInfo) SendCleanupTopicsNotice(topics []string) error {
	return backend.sendTopicNotice(strings.Join(topics, ","), false)
}

func (backend *backendInfo) sendTopicNotice(topic string, added bool) error {
	formData := url.Values{}
	formData.Set("channels", topic)
	if added {
		formData.Set("added", "t")
	} else {
		formData.Set("added", "f")
	}

	sealedForm, err := backend.secureForm.Seal(formData)
	if err != nil {
		return err
	}

	resp, err := backend.HTTPClient.PostForm(backend.addTopicURL, sealedForm)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		respBytes, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return ErrBackendNotOK{Code: resp.StatusCode, Response: fmt.Sprintf("(error reading non-2xx response): %s", err.Error())}
		}
		return ErrBackendNotOK{Code: resp.StatusCode, Response: string(respBytes)}
	}

	backend.lastSuccessLock.Lock()
	defer backend.lastSuccessLock.Unlock()
	backend.lastSuccess[bPathAddTopic] = time.Now().UTC()

	return nil
}

func httpError(statusCode int) error {
	return fmt.Errorf("backend http error: %d", statusCode)
}

// GenerateKeys generates a new NaCl keypair for the server and writes out the default configuration file.
func GenerateKeys(outputFile, serverID, theirPublicStr string) {
	var err error
	output := ConfigFile{
		ListenAddr:      "0.0.0.0:8001",
		SSLListenAddr:   "0.0.0.0:443",
		BackendURL:      "http://localhost:8002/ffz",
		MinMemoryKBytes: defaultMinMemoryKB,
	}

	output.ServerID, err = strconv.Atoi(serverID)
	if err != nil {
		log.Fatal(err)
	}

	ourPublic, ourPrivate, err := box.GenerateKey(rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	output.OurPublicKey, output.OurPrivateKey = ourPublic[:], ourPrivate[:]

	if theirPublicStr != "" {
		reader := base64.NewDecoder(base64.StdEncoding, strings.NewReader(theirPublicStr))
		theirPublic, err := ioutil.ReadAll(reader)
		if err != nil {
			log.Fatal(err)
		}
		output.BackendPublicKey = theirPublic
	}

	bytes, err := json.MarshalIndent(output, "", "\t")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(bytes))
	err = ioutil.WriteFile(outputFile, bytes, 0600)
	if err != nil {
		log.Fatal(err)
	}
}
