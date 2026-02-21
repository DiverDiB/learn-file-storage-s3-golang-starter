package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {

	// Set upload limit of 1 GB
	const maxUploadSize = 1 << 30 // 1 GB
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)

	// Extract the videoID from the URL path
	videoIDString := r.PathValue("videoID")

	// Parse as UUID
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	// Authenticate the user to get userID
	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	fmt.Println("uploading video for video", videoID, "by user", userID)

	// Get the video metadata from the database
	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video metadata", err)
		return
	}

	// Check if the authenticated user is the owner of the video
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "You don't have permission to upload a video for this video ID", fmt.Errorf("user %s doesn't own video %s", userID, videoID))
		return
	}

	// Parse the uploaded file from the form data
	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't get video file from form data", err)
		return
	}
	defer file.Close()

	// Validate the media type and get the file extension using mime.ParseMediaType
	mediaType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse media type", err)
		return
	}

	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Unsupported media type", fmt.Errorf("unsupported media type: %s", mediaType))
		return
	}

	// Save the uploaded file to a temporary location on disk
	tempFile, err := os.CreateTemp("", "tubely-upload-*.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create temp file", err)
		return
	}
	defer os.Remove(tempFile.Name()) // Clean up temp file after processing
	defer tempFile.Close()

	// Copy the uploaded file to the temporary file
	_, err = io.Copy(tempFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't copy uploaded file to temp file", err)
		return
	}

	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video aspect ratio", err)
		return
	}
	var aspectString string

	switch aspectRatio {
	case "16:9":
		aspectString = "landscape"
	case "9:16":
		aspectString = "portrait"
	default:
		aspectString = "other"
	}

	// Reset the file pointer to the beginning of the file for future use
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't reset file pointer", err)
		return
	}

	// Upload the video file to S3
	// Generate a random filename for the video in S3 to avoid collisions and security issues
	key := make([]byte, 32)
	_, err = rand.Read(key)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate random bytes for thumbnail filename", err)
		return
	}
	// Convert to random base64 string
	randomName := base64.RawURLEncoding.EncodeToString(key)

	s3Key := fmt.Sprintf("%s/videos/%s.mp4", aspectString, randomName)

	_, err = cfg.s3Client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(s3Key),
		Body:        tempFile,
		ContentType: aws.String(mediaType),
	})

	// Update the videoURL in the database to point to the S3 URL
	videoURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, s3Key)
	video.VideoURL = &videoURL

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video metadata with video URL", err)
		return
	}

	respondWithJSON(w, http.StatusOK, map[string]string{
		"videoURL": videoURL,
	})
}

func getVideoAspectRatio(filePath string) (string, error) {
	// Run ffprobe to get the video's width and height
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)

	// Set Stdout to a pointer to a new bytes.Buffer
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return "", err
	}

	// Unmarshal the output into a struct
	type FFProbeOutput struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
	}

	var ffprobeOutput FFProbeOutput
	err = json.Unmarshal(out.Bytes(), &ffprobeOutput)
	if err != nil {
		return "", err
	}

	if len(ffprobeOutput.Streams) == 0 {
		return "", errors.New("no streams found in ffprobe output")
	}

	fmt.Printf("ffprobe output: %+v\n", ffprobeOutput)
	// Return the aspect ratio as a string in the format "width:height"

	// Calculate the actual ratio of the video
	ratio := float64(ffprobeOutput.Streams[0].Width) / float64(ffprobeOutput.Streams[0].Height)

	// Check for Landscape (16:9)
	if math.Abs(ratio-(16.0/9.0)) < 0.1 {
		return "16:9", nil
	}

	// Check for Portrait (9:16)
	if math.Abs(ratio-(9.0/16.0)) < 0.1 {
		return "9:16", nil
	}

	// If it's anything else (like a square 1:1 or old 4:3)
	return "other", nil
}
