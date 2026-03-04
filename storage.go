package main

import (
	"encoding/json"
	"log"
	"os"
	"sync"
)

type PageData struct {
	URL       string   `json:"url"`
	Title     string   `json:"title"`
	Links     []string `json:"discovered_links"`
	Timestamp string   `json:"timestamp"`
}

type Storage struct {
	mu    sync.Mutex
	file  *os.File
	enc   *json.Encoder
	first bool
}

func NewStorage(filename string) (*Storage, error) {
	file, err := os.Create(filename)
	if err != nil {
		return nil, err
	}

	// Write start of JSON array
	_, err = file.WriteString("[\n")
	if err != nil {
		return nil, err
	}

	return &Storage{
		file:  file,
		enc:   json.NewEncoder(file),
		first: true,
	}, nil
}

func (s *Storage) Save(data PageData) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Handle JSON array formatting
	if !s.first {
		s.file.WriteString(",\n")
	} else {
		s.first = false
	}

	if err := s.enc.Encode(data); err != nil {
		log.Printf("Failed to encode data for %s: %v", data.URL, err)
	}
}

func (s *Storage) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Close JSON array
	_, err := s.file.WriteString("\n]")
	if err != nil {
		return err
	}
	return s.file.Close()
}
