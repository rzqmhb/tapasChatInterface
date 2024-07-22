package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
	ReadBufferSize: 2048,
	WriteBufferSize: 2048,
}

var clients = make(map[*websocket.Conn]bool)
var broadcast = make(chan string)

type WebsocketRequest struct {
	Text string `json:"text"`
	File string `json:"file,omitempty"`
}

type AIModelConnector struct {
	Client *http.Client
}

type Inputs struct {
	Table map[string][]string `json:"table"`
	Query string              `json:"query"`
}

type Response struct {
	Answer      string   `json:"answer"`
	Coordinates [][]int  `json:"coordinates"`
	Cells       []string `json:"cells"`
	Aggregator  string   `json:"aggregator"`
}

func init()  {
	err := godotenv.Load()
	if err != nil {
		panic(err)
	}
}

func CsvToSlice(data string) (map[string][]string, error) {
	var result = map[string][]string{}

	csvLines := strings.Split(data, "\n")
	headers := strings.Split(csvLines[0], ",")
	for _, header := range headers {
		result[header] = []string{}
	}
	
	for _, line := range csvLines[1:] {
		words := strings.Split(line, ",")
		for i, word := range words {
			result[headers[i]] = append(result[headers[i]], word)
		}
	}

	return result, nil // TODO: replace this
}

func ReadCSV(filePath string) string {
	var result = ""
	file, err := os.Open(filePath)
	if err != nil {
		panic(err)
	}
	defer file.Close()

	csvReader := csv.NewReader(file)
	records, err :=csvReader.ReadAll()
	if err != nil {
		panic(err)
	}
	
	for j, record := range records {
		for i, word := range record {
			if i == len(record)-1 {
				result += word
				break
			}
			result += word +","
		}
		if j == len(records)-1 {
			break
		}
		result += "\n"
	}
	return result
}

func (c *AIModelConnector) ConnectAIModel(payload interface{}, token string) (Response, error) {
	var responseInStruct Response

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return Response{}, fmt.Errorf("error encoding to json: %s", err)
	}

	body := bytes.NewReader(payloadBytes)
	request, err := http.NewRequest(http.MethodPost, "https://api-inference.huggingface.co/models/google/tapas-large-finetuned-wtq", body)
	if err != nil {
		return Response{}, fmt.Errorf("error creating http request: %s", err)
	}

	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))

	response, err := c.Client.Do(request)
	if err != nil {
		return Response{}, fmt.Errorf("error while making request: %s", err)
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		return Response{}, fmt.Errorf("error reading responde body: %s", err)
	}

	if response.StatusCode != http.StatusOK {
		if response.StatusCode == 503 {
			return Response{}, fmt.Errorf("tapas model is still loading")
		}
		return Response{}, fmt.Errorf("status code not 200")
	}

	err = json.Unmarshal(responseBody, &responseInStruct)
	if err != nil {
		return Response{}, fmt.Errorf("error decoding response: %s", err)
	}
	return responseInStruct, nil // TODO: replace this
}

func RunCLIINterface(huggingFaceToken string) {
	reader := bufio.NewReader(os.Stdin)
	result := ReadCSV("data-series.csv")
	maps, _ := CsvToSlice(result)
	connector := AIModelConnector{
		Client: http.DefaultClient,
	}


	fmt.Print(`

====================================
Languange Model Tapas CLI Interface
====================================

Silahkan masukkan pertanyaan: (masukkan 'exit' untuk keluar)

`)

	for true {
		fmt.Print("Pertanyaan \t\t: ")
		query, _ := reader.ReadString('\n')
		query = query[:len(query)-2]
		
		if query == "exit" {
			break
		}

		input := Inputs{
			Table: maps,
			Query: query,
		}

		httpResponse, err := connector.ConnectAIModel(input, huggingFaceToken)
		if err != nil {
			if err.Error() == "tapas model is still loading" {
				fmt.Println("Model Tapas sedang dimuat...")
				continue
			}
			panic(err)	
		}
		fmt.Printf("Jawaban Model Tapas\t: %s\n", httpResponse.Answer)
	}
}

func RunWebInterface(huggingFaceToken string)  {
	mux := http.DefaultServeMux

	mux.Handle("/tapas/chat", TapasChatPageHandler())
	mux.Handle("/ws", WebSocketHandler(huggingFaceToken))

	go HandleMessages()

	fmt.Println("Server is running on port 9090")
	fmt.Println("Access homepage here: http://localhost:9090/tapas/chat")
	http.ListenAndServe("localhost:9090", mux)
}

func TapasChatPageHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		filePath := path.Join("pages", "chat_page.html")
		tmpl, err := template.ParseFiles(filePath)
		if err != nil {
			http.Error(w, fmt.Sprintf("error parsing html: %s", err.Error()), http.StatusInternalServerError)
		}

		var data = map[string]string{
			"title":"Ask Tapas",
		}
		
		err = tmpl.Execute(w, data)
		if err != nil {
			log.Println(err)
			http.Error(w, fmt.Sprintf("error executing html: %s", err.Error()), http.StatusInternalServerError)
		}
	}
}

func WebSocketHandler(token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Println(err.Error())
			http.Error(w, fmt.Sprintf("error executing websocket: %s", err.Error()), http.StatusInternalServerError)
		}
		defer ws.Close()

		clients[ws] = true

		for {
			var csvInString string
			var csvInMap map[string][]string
			var requestBody WebsocketRequest

			var getAndSendTapasAnswer = func (filePath string, query string)  {
				csvInString = ReadCSV(filePath)
				csvInMap, _ = CsvToSlice(csvInString)
				connector := AIModelConnector{
					Client: http.DefaultClient,
				}

				input := Inputs{
					Table: csvInMap,
					Query: query,
				}
		
				httpResponse, err := connector.ConnectAIModel(input, token)
				if err != nil {
					if err.Error() == "tapas model is still loading" {
						broadcast <- "Loading Tapas Model..."
						return
					}
					broadcast <- "internal server error"	
					return
				}
				broadcast <- httpResponse.Answer
			}

			err := ws.ReadJSON(&requestBody)
			if err != nil {
				log.Println(err.Error())
				http.Error(w, fmt.Sprintf("error retrieving request: %s", err.Error()), http.StatusInternalServerError)
			}

			if requestBody.File != "" {
				err := EmptyTMPDir()
				if err != nil {
					log.Fatal(err)
					break
				}

				filePath, err := SaveFile(requestBody.File)
				if err != nil {
					log.Printf("error saving file: %s\n", err)
					delete(clients, ws)
					break
				}

				getAndSendTapasAnswer(filePath, requestBody.Text)
			} else {
				files, err := os.ReadDir("tmp")
				if err != nil {
					log.Printf("error getting file: %s\n", err)
					delete(clients, ws)
					break
				}

				if len(files) != 1  {
					broadcast <- "No CSV file available/provided as a context."
					continue
				}

				fileName := files[0].Name()
				filePath := filepath.Join("tmp", fileName)

				getAndSendTapasAnswer(filePath, requestBody.Text)
			}
		}
	}
}

func HandleMessages()  {
	for {
		message := <- broadcast
		for client := range clients {
			msgInMap := map[string]string{
				"message": message,
			}
			err := client.WriteJSON(msgInMap)
			if err != nil {
				log.Printf("error sending message: %s", err)
                client.Close()
                delete(clients, client)
			}
		}
	}
}

func SaveFile(encodedFile string) (string, error) {
	if strings.HasPrefix(encodedFile, "data:") {
		parts := strings.SplitN(encodedFile, ",", 2)
		if len(parts) != 2 {
			return "", fmt.Errorf("invalid data URL")
		}
		encodedFile = parts[1]
	}

	fileData, err := base64.StdEncoding.DecodeString(encodedFile)
	if err != nil {
		return "", fmt.Errorf("error decoding file: %s", err)
	}

	tmpFile, err := os.CreateTemp("tmp", "upload-*.csv")
	// tmpFilePath := path.Join("tmp", tmpFile.Name())
	if err != nil {
		return "", fmt.Errorf("error creating temp file: %s", err)
	}
	defer tmpFile.Close()

	_, err = tmpFile.Write(fileData)
	if err != nil {
		return "", fmt.Errorf("error saving file data: %s", err)
	}
	
	return tmpFile.Name(), nil
}

func EmptyTMPDir() error {
	files, err := os.ReadDir("tmp")
	if err != nil {
		return err
	}
	
	for _, file := range files {
		os.RemoveAll(path.Join("tmp", file.Name()))
	}
	return nil
}

func main() {
	// TODO: answer here
	reader := bufio.NewReader(os.Stdin)
	err := godotenv.Load()
	if err != nil {
		panic(err)
	}
	token := os.Getenv("HUGGINGFACE_TOKEN")

	fmt.Print(`
===============================
Languange Model Tapas Interface
===============================

Interface yang tersedia:
1. CLI
2. Web

Pilih interface untuk dijalankan: `)

	interfaceChoice, _ := reader.ReadString('\n')
	interfaceChoice =  interfaceChoice[:len(interfaceChoice)-2]

	if interfaceChoice == "1" {
		RunCLIINterface(token)
	} else {
		RunWebInterface(token)
	}
}
