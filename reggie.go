package main

import (
	"encoding/json"
	"io/ioutil"
	"log"
	"mime"
	"net/http"
	"sync"

	"fmt"

	"os/exec"

	"os"

	"bytes"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/docker/distribution/notifications"
)

var seen = make(map[string]bool)
var mu sync.Mutex

var peers []string
var torrentClient *torrent.Client

func main() {

	var clientConfig torrent.Config
	clientConfig.Seed = true
	clientConfig.DisableTrackers = true
	clientConfig.NoDHT = true
	var err error
	torrentClient, err = torrent.NewClient(&clientConfig)
	if err != nil {
		log.Printf("error creating client: %s", err)
		return
	}

	//get peers from somewhere ideally
	//remove self from list?
	peers = append(peers, "host2:8000")
	http.HandleFunc("/registryNotifications", regHandler)
	http.HandleFunc("/torrent", torrentHandler)

	//Registry expects to find us on localhost:8000
	log.Fatal(http.ListenAndServe("0.0.0.0:8000", nil))
}

func torrentHandler(w http.ResponseWriter, r *http.Request) {

	defer r.Body.Close()
	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		log.Printf("unexpected request method: %v", r.Method)
		return
	}

	// Extract the content type and make sure it matches
	contentType := r.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		log.Printf("error parsing media type: %v, contenttype=%q", err, contentType)
		return
	}

	if mediaType != "application/json" {
		w.WriteHeader(http.StatusUnsupportedMediaType)
		log.Printf("incorrect media type: %q != %q", mediaType, "application/json")
		return
	}

	var mi metainfo.MetaInfo
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&mi); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		log.Printf("error decoding request body: %v", err)
		return
	}

	//log.Printf("got some metadata %v\n", mi.)
	log.Printf("Got torrent, retrieving")
	seedTorrent(&mi, loadImageFromTorrent)
	TODO: check status and add peers to torrent

}

func loadImageFromTorrent(t *torrent.Torrent) {

	//should be a single file
	log.Printf("Got image file: %s\n", t.Files()[0].DisplayPath())
	t.
	err := exec.Command("docker", "load", t.Files()[0].Path()).Run()
	if err != nil {
		log.Printf("Failed load %v\n", err)
		return

	}
	log.Println("Loaded")

}

func regHandler(w http.ResponseWriter, r *http.Request) {

	defer r.Body.Close()
	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		log.Printf("unexpected request method: %v", r.Method)
		return
	}

	// Extract the content type and make sure it matches
	contentType := r.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		log.Printf("error parsing media type: %v, contenttype=%q", err, contentType)
		return
	}

	if mediaType != notifications.EventsMediaType {
		w.WriteHeader(http.StatusUnsupportedMediaType)
		log.Printf("incorrect media type: %q != %q", mediaType, notifications.EventsMediaType)
		return
	}

	var envelope notifications.Envelope
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&envelope); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		log.Printf("error decoding request body: %v", err)
		return
	}

	evs := envelope.Events
	for _, e := range evs {

		mu.Lock()
		if !seen[e.ID] && e.Action == "push" &&
			e.Target.MediaType == "application/vnd.docker.distribution.manifest.v2+json" {
			seen[e.ID] = true
			logEvent(e)
			downloadAndSeedImage(e.Target.Repository, e.Target.Tag)
		}
		mu.Unlock()
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "OK\n")

	//filter on action push and mediatype manifest?
	//ignore layers for moment
	//then pull given image
	//call ronnie / wellbourne?

}

func downloadAndSeedImage(repo string, tag string) {
	//Could directly use API rather than docker pull?
	imageName := fmt.Sprintf("%s/%s:%s", "localhost:5000", repo, tag)
	err := exec.Command("docker", "pull", imageName).Run()
	if err != nil {
		log.Printf("Failed pull %v\n", err)
		return

	}
	log.Println("Pulled")

	tmpfile, err := ioutil.TempFile("", repo+tag)
	if err != nil {
		log.Print(err)
		return
	}
	tmpfile.Close()
	err = exec.Command("docker", "save", "-o", tmpfile.Name(), imageName).Run()
	if err != nil {
		log.Printf("Failed save %v\n", err)
		return
	}
	log.Println("Saved")
	mi := createTorrent(tmpfile)
	seedTorrent(&mi, notifyPeers)
	log.Println("Seeded")

	//log.Printf("torrent: %v\n", mi)

}

func seedTorrent(mi *metainfo.MetaInfo, cb func(*torrent.Torrent)) {

	//Hmm, will need to do this separately and send torrents dynamically
	//but for the moment...

	t, err := torrentClient.AddTorrent(mi)
	if err != nil {
		log.Printf("error adding torrent: %s", err)
		return
	}
	go func() {
		<-t.GotInfo()
		t.DownloadAll()
		cb(t)
	}()

	/*
		Then need to think about how to ping others and d/load...
		Come up with way to test this using containers on same host
		may require multiple registry instances.
		Put reggie and reg in same container?
	*/

}

func notifyPeers(t *torrent.Torrent) {

	mi := t.Metainfo()
	for _, p := range peers {

		url := fmt.Sprintf("http://%s/torrent", p)
		data, err := json.Marshal(mi)
		if err != nil {
			log.Printf("Failed to create JSON %v\n", err)
		}
		//log.Printf("Sending torrent JSON: %s", string(data))

		r := bytes.NewReader(data)

		http.Post(url, "application/json", r)

	}

}

func createTorrent(f *os.File) metainfo.MetaInfo {
	mi := metainfo.MetaInfo{}

	mi.SetDefaults()
	info := metainfo.Info{
		PieceLength: 256 * 1024,
	}

	err := info.BuildFromFilePath(f.Name())
	if err != nil {
		log.Fatal(err)
	}
	mi.InfoBytes, err = bencode.Marshal(info)
	if err != nil {
		log.Fatal(err)
	}

	return mi

}

func logEvent(e notifications.Event) {

	log.Println("EVENT")
	log.Printf("ACTION: %v\n", e.Action)
	log.Printf("ACTOR: %v\n", e.Actor)
	log.Printf("ID: %v\n", e.ID)
	log.Printf("REQUEST: %v\n", e.Request)
	log.Printf("SOURCE: %v\n", e.Source)
	log.Printf("TARGET DESCRIPTOR: %v\n", e.Target.Descriptor)
	log.Printf("TARGET FROMREPO: %v\n", e.Target.FromRepository)
	log.Printf("TARGET MEDIATYPE: %v\n", e.Target.MediaType)
	log.Printf("TARGET REPO: %v\n", e.Target.Repository)
	log.Printf("TARGET TAG: %v\n", e.Target.Tag)
	log.Printf("TARGET URLs: %v\n", e.Target.URLs)
	log.Printf("TARGET URL: %v\n", e.Target.URL)
	log.Printf("TIMESTAMP: %v\n", e.Timestamp)
}
