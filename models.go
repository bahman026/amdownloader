package main

type Track struct {
	Index    int
	Link     string
	Name     string
	Artist   string
	Duration string
	Thumb    string
	Album    string
}

type ResolveRequest struct {
	SongName string
	Artist   string
	URL      string
	Quality  int
}

type ResolveResponse struct {
	DLink    string `json:"dlink"`
	Status   string `json:"status"`
	Comments string `json:"comments"`
	Error    string `json:"error"`
}

type SaveID3Request struct {
	URL    string
	Name   string
	Artist string
	Album  string
	Thumb  string
}