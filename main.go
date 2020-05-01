package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gorilla/mux"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

type Scan struct {
	ID        primitive.ObjectID `json:"id" bson:"_id"`
	URL       string             `json:"url" bson:"url"`
	JSON      string             `json:"json" bson:"json"`
	HTML      string             `json:"html" bson:"html"`
	CreatedAt time.Time          `json:"created_at" bson:"created_at"`
}

type App struct {
	Router *mux.Router
	DB     *mongo.Client
}

// "mongodb://localhost:27017"
func NewApp(mongoURI string) *App {
	a := new(App)
	a.CreateMongoClient(mongoURI)
	a.SetupRoutes()
	return a
}

func (a *App) SetupRoutes() {
	a.Router = mux.NewRouter()
	a.Router.HandleFunc("/scans", a.getScans).Methods("GET")
	a.Router.HandleFunc("/scans", a.createScan).Methods("POST")
	a.Router.HandleFunc("/scans/{id}", a.getScan).Methods("GET")
	a.Router.HandleFunc("/scans/{id}", a.deleteScan).Methods("DELETE")
}

func (a *App) Run(address string) {
	log.Print("Listening on :8000")
	http.ListenAndServe(address, a.Router)
}

func main() {
	mongoURI := os.Getenv("MONGO_URI")
	if mongoURI == "" {
		mongoURI = "mongodb://localhost:27017"
	}
	a := NewApp(mongoURI)
	a.Run(":8000")
}

func (a *App) CreateMongoClient(mongoURI string) {
	var err error
	a.DB, err = mongo.NewClient(options.Client().ApplyURI(mongoURI))
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err = a.DB.Connect(ctx)
	if err != nil {
		log.Fatal(err)
	}
}

func (a *App) getScans(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	scans := []Scan{}
	collection := a.DB.Database("speedster").Collection("scans")
	c := context.TODO()
	cursor, err := collection.Find(c, bson.D{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := cursor.All(c, &scans); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	json.NewEncoder(w).Encode(&scans)
}

type malformedRequest struct {
	status int
	msg    string
}

func (mr *malformedRequest) Error() string {
	return mr.msg
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst interface{}) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1048576)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	err := dec.Decode(&dst)
	if err != nil {
		var syntaxError *json.SyntaxError
		var unmarshalTypeError *json.UnmarshalTypeError

		switch {
		case errors.As(err, &syntaxError):
			msg := fmt.Sprintf("Request body contains badly-formed JSON (at position %d)", syntaxError.Offset)
			return &malformedRequest{status: http.StatusBadRequest, msg: msg}

		case errors.Is(err, io.ErrUnexpectedEOF):
			msg := fmt.Sprintf("Request body contains badly-formed JSON")
			return &malformedRequest{status: http.StatusBadRequest, msg: msg}

		case errors.As(err, &unmarshalTypeError):
			msg := fmt.Sprintf("Request body contains an invalid value for the %q field (at position %d)", unmarshalTypeError.Field, unmarshalTypeError.Offset)
			return &malformedRequest{status: http.StatusBadRequest, msg: msg}

		case strings.HasPrefix(err.Error(), "json: unknown field "):
			fieldName := strings.TrimPrefix(err.Error(), "json: unknown field ")
			msg := fmt.Sprintf("Request body contains unknown field %s", fieldName)
			return &malformedRequest{status: http.StatusBadRequest, msg: msg}

		case errors.Is(err, io.EOF):
			msg := "Request body must not be empty"
			return &malformedRequest{status: http.StatusBadRequest, msg: msg}

		case err.Error() == "http: request body too large":
			msg := "Request body must not be larger than 1MB"
			return &malformedRequest{status: http.StatusRequestEntityTooLarge, msg: msg}

		default:
			return err
		}
	}

	if dec.More() {
		msg := "Request body must only contain a single JSON object"
		return &malformedRequest{status: http.StatusBadRequest, msg: msg}
	}

	return nil
}

func (a *App) createScan(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var scan Scan
	err := decodeJSONBody(w, r, &scan)
	if err != nil {
		var mr *malformedRequest
		if errors.As(err, &mr) {
			http.Error(w, mr.msg, mr.status)
		} else {
			log.Println(err.Error())
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
		return
	}
	scan.ID = primitive.NewObjectID()
	scan.CreatedAt = time.Now()
	log.Printf("Decoded json from HTTP body. Scan: %+v", scan)

	html, jsonResult := runLightHouse(scan.URL, "/home/chrome/reports/speedster")
	scan.HTML = string(html)
	scan.JSON = string(jsonResult)

	ctx, _ := context.WithTimeout(context.Background(), 5*time.Second)
	collection := a.DB.Database("speedster").Collection("scans")
	log.Print("Inserting Scan:", scan.ID, scan.URL)
	_, err = collection.InsertOne(ctx, scan)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	json.NewEncoder(w).Encode(&scan)
}

func runLightHouse(url, outputPath string) (html, json []byte) {
	// lighthouse --chrome-flags="--headless" $URL --output="html" --output=json --output-path=/tmp/$URL
	cmd := exec.Command("lighthouse", "--chrome-flags=\"--headless\"", url,
		"--output=json", "--output=html", "--output-path="+outputPath)
	var stdOut, stdErr bytes.Buffer
	cmd.Stdout = &stdOut
	cmd.Stderr = &stdErr
	log.Printf("Running command %+v", cmd)

	if err := cmd.Run(); err != nil {
		log.Print(err)
		return nil, nil
	}
	defer os.Remove(outputPath + ".report.json")
	defer os.Remove(outputPath + ".report.html")

	var err error
	json, err = ioutil.ReadFile(outputPath + ".report.json")
	if err != nil {
		log.Print("Error reading lighthouse json output file:", err)
		return nil, nil
	}
	html, err = ioutil.ReadFile(outputPath + ".report.html")
	if err != nil {
		log.Print("Error reading lighthouse html output file:", err)
		return nil, nil
	}

	return html, json
}

func (a *App) getScan(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	params := mux.Vars(r)
	var scan Scan
	collection := a.DB.Database("speedster").Collection("scans")
	oid, err := primitive.ObjectIDFromHex(params["id"])
	err = collection.FindOne(context.Background(), bson.M{"_id": oid}).Decode(&scan)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	json.NewEncoder(w).Encode(&scan)
}

func (a *App) deleteScan(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	params := mux.Vars(r)
	oid, err := primitive.ObjectIDFromHex(params["id"])
	if err != nil {
		http.Error(w, "Invalid id: "+params["id"], http.StatusBadRequest)
		return
	}

	result, err := a.DB.Database("speedster").Collection("scans").DeleteOne(context.TODO(), bson.M{"_id": oid}, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if result.DeletedCount == 1 {
		json.NewEncoder(w).Encode(&Scan{})
	} else if result.DeletedCount == 0 {
		http.Error(w, "Scan with id "+params["id"]+" did not exist", http.StatusBadRequest)
	} else {
		http.Error(w, "Multiple scans were deleted. Contact support.", http.StatusBadRequest)
	}
}