package geoserve

import (
	"encoding/json"
	gerrors "errors"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/golang/groupcache/lru"
	"github.com/mholt/archiver/v3"
	geoip2 "github.com/oschwald/geoip2-golang"

	errors "github.com/getlantern/errors"

	"github.com/getlantern/golog"
)

const (
	CacheSize = 50000
)

var (
	log            = golog.LoggerFor("go-geoserve")
	errNotModified = gerrors.New("unmodified")
)

// GeoServer is a server for IP geolocation information
type GeoServer struct {
	db       *geoip2.Reader
	dbURL    string
	cache    *lru.Cache
	cacheGet chan get
	dbUpdate chan *geoip2.Reader
}

// get encapsulates a request to geolocate an ip address
type get struct {
	ip   string
	resp chan []byte
}

// NewServer constructs a new GeoServer using the (optional) uncompressed dbFile.
// If dbFile is "", then this will fetch the latest GeoLite2-City database from
// the specified DBURL
func NewServer(dbFile, dbURL string) (server *GeoServer, err error) {
	server = &GeoServer{
		cache:    lru.New(CacheSize),
		cacheGet: make(chan get, 10000),
		dbUpdate: make(chan *geoip2.Reader),
	}
	var lastModified time.Time
	if dbFile != "" {
		server.db, lastModified, err = readDbFromFile(dbFile)
		if err != nil {
			return nil, errors.New("unable to read DB from file: %v", err)
		}
	} else {
		server.dbURL = dbURL
		server.db, lastModified, err = readDbFromWeb(server.dbURL, time.Time{})
		if err != nil {
			return nil, errors.New("unable to read DB from web: %v", err)
		}
	}
	go server.run()
	if len(dbURL) > 0 {
		go server.keepDbCurrent(lastModified)
	}
	return
}

// Handle is used to handle requests from an HTTP server. basePath is the path
// at which the containing request handler is registered, and is used to extract
// the ip address from the remainder of the path. allowOrigin is the cors
// response config, if not empty it is written to the response header.
func (server *GeoServer) Handle(resp http.ResponseWriter, req *http.Request, basePath string, allowOrigin string) {
	if allowOrigin != "" {
		(resp).Header().Set("Access-Control-Allow-Origin", allowOrigin)
	}
	path := strings.Replace(req.URL.Path, basePath, "", 1)
	// Use path as ip
	ip := path
	if ip == "" {
		// When no path supplied, grab remote address or X-Forwarded-For
		ip = clientIpFor(req)
	}
	g := get{ip, make(chan []byte)}
	server.cacheGet <- g
	jsonData := <-g.resp
	if jsonData == nil {
		resp.WriteHeader(500)
	} else {
		resp.Header().Set("X-Reflected-Ip", ip)
		resp.Write(jsonData)
	}
}

// run runs the geolocation routine which takes care of looking up values from
// the cache, updating the cache and udpating the database when a new version is
// available.
func (server *GeoServer) run() {
	for {
		select {
		case g := <-server.cacheGet:

			if cached, found := server.cache.Get(g.ip); found {
				log.Trace("Cache hit")
				g.resp <- cached.([]byte)
			} else {
				jsonData, err := server.lookupDB(g.ip)
				if err != nil {
					log.Error(err)
				} else {
					server.cache.Add(g.ip, jsonData)
				}
				g.resp <- jsonData
			}
		case db := <-server.dbUpdate:
			if server.db != nil {
				log.Debug("Closing old database")
				server.db.Close()
			}
			log.Debug("Applying new database")
			server.db = db
			log.Debug("Clearing cached lookups")
			server.cache = lru.New(CacheSize)
		}
	}
}

func (server *GeoServer) lookupDB(ip string) ([]byte, error) {
	geoData, err := server.db.Country(net.ParseIP(ip))
	if err != nil {
		return nil, errors.New("Unable to look up ip address %s: %s", ip, err)
	}
	jsonData, err := json.Marshal(geoData)
	if err != nil {
		return nil, errors.New("Unable to encode json response for ip address: %s", ip)
	}
	return jsonData, nil
}

// keepDbCurrent checks the MaxMind database URL every hour and downloads it if it's
// newer and submits it to server.dbUpdate for the run() routine to pick up.
func (server *GeoServer) keepDbCurrent(lastModified time.Time) {
	for {
		time.Sleep(1 * time.Hour)
		db, modifiedTime, err := readDbFromWeb(server.dbURL, lastModified)
		if err == errNotModified {
			continue
		}
		if err != nil {
			log.Errorf("Unable to update database from web: %s", err)
			continue
		}
		lastModified = modifiedTime
		server.dbUpdate <- db
	}
}

// readDbFromFile reads the MaxMind database and timestamp from a file
func readDbFromFile(dbFile string) (*geoip2.Reader, time.Time, error) {
	dbData, err := ioutil.ReadFile(dbFile)
	if err != nil {
		return nil, time.Time{}, errors.New("Unable to read db file %s: %s", dbFile, err)
	}
	fileInfo, err := os.Stat(dbFile)
	if err != nil {
		return nil, time.Time{}, errors.New("Unable to stat db file %s: %s", dbFile, err)
	}
	lastModified := fileInfo.ModTime()
	db, err := openDb(dbData)
	if err != nil {
		return nil, time.Time{}, errors.New("unable to open db from file %s: %v", dbFile, err)
	} else {
		return db, lastModified, nil
	}
}

// readDbFromWeb reads the MaxMind database and timestamp from the web
func readDbFromWeb(url string, ifModifiedSince time.Time) (*geoip2.Reader, time.Time, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, time.Time{}, errors.New("unable to construct HTTP request for file: %v", err)
	}
	req.Header.Add("If-Modified-Since", ifModifiedSince.Format(http.TimeFormat))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, time.Time{}, errors.New("Unable to get database from %s: %s", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil, time.Time{}, errNotModified
	}
	if resp.StatusCode != http.StatusOK {
		return nil, time.Time{}, errors.New("unexpected HTTP status %v", resp.Status)
	}
	lastModified, err := getLastModified(resp)
	if err != nil {
		return nil, time.Time{}, errors.New("Unable to parse Last-Modified header %s: %s", lastModified, err)
	}

	unzipper := archiver.NewTarGz()
	err = unzipper.Open(resp.Body, 0)
	if err != nil {
		return nil, time.Time{}, errors.New("unable to unzip tar.gz: %v", err)
	}
	defer unzipper.Close()
	for {
		f, err := unzipper.Read()
		if err != nil {
			return nil, time.Time{}, errors.New("unable to read from tar.gz: %v", err)
		}
		if f.Name() == "GeoLite2-Country.mmdb" {
			dbData, err := ioutil.ReadAll(f)
			if err != nil {
				return nil, time.Time{}, errors.New("unable to read GeoLite2-Country.mmdb: %v", err)
			}
			db, err := openDb(dbData)
			if err != nil {
				return nil, time.Time{}, errors.New("unable to open db: %v", err)
			}
			return db, lastModified, nil
		}
	}
}

// getLastModified parses the Last-Modified header from a response
func getLastModified(resp *http.Response) (time.Time, error) {
	lastModified := resp.Header.Get("Last-Modified")
	return http.ParseTime(lastModified)
}

// openDb opens a MaxMind in-memory db using the geoip2.Reader
func openDb(dbData []byte) (*geoip2.Reader, error) {
	db, err := geoip2.FromBytes(dbData)
	if err != nil {
		return nil, errors.New("Unable to open database: %s", err)
	} else {
		return db, nil
	}
}

func clientIpFor(req *http.Request) string {
	// Client requested their info
	clientIp := req.Header.Get("X-Forwarded-For")
	if clientIp == "" {
		clientIp = strings.Split(req.RemoteAddr, ":")[0]
	}
	// clientIp may contain multiple ips, use the first
	ips := strings.Split(clientIp, ",")
	return strings.TrimSpace(ips[0])
}
