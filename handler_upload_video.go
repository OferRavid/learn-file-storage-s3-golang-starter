package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/OferRavid/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
)

const maxVidMemory = 1 << 30

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxVidMemory)

	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

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

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to fetch video from database", err)
		return
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "User authenticated is not the user who uploaded the video", fmt.Errorf("UserID doesn't match video's UserID"))
		return
	}

	videoFile, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Failed to fetch file data", err)
		return
	}
	defer videoFile.Close()

	mediaType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if mediaType == "" || mediaType != "video/mp4" || err != nil {
		respondWithError(w, http.StatusBadRequest, "Missing or wrong Content-Type for video", nil)
		return
	}

	tmpVidFile, err := os.CreateTemp("", "tubely_upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to create file on server", err)
		return
	}
	defer os.Remove(tmpVidFile.Name())
	defer tmpVidFile.Close()

	_, err = io.Copy(tmpVidFile, videoFile)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to copy data to file", err)
		return
	}

	tmpVidFile.Seek(0, io.SeekStart)
	tmpProcessedVidFile, err := processVideoForFastStart(tmpVidFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, err.Error(), err)
		return
	}

	processedVidFile, err := os.Open(tmpProcessedVidFile)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "failed to open processed video file", err)
	}

	defer os.Remove(tmpProcessedVidFile)
	defer processedVidFile.Close()

	directory := ""
	aspectRatio, err := getVideoAspectRatio(tmpVidFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get aspect ratio for video", err)
		return
	}
	switch aspectRatio {
	case "16:9":
		directory = "landscape"
	case "9:16":
		directory = "portrait"
	default:
		directory = "other"
	}

	assetPath := filepath.Join(directory, getAssetPath(mediaType))
	videoURL := cfg.getObjectURL(assetPath)
	video.VideoURL = &videoURL

	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(assetPath),
		Body:        processedVidFile,
		ContentType: aws.String(mediaType),
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed upload to s3 bucket", err)
		return
	}

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video in database", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	var cmdBuffer bytes.Buffer
	cmd.Stdout = &cmdBuffer
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("failed to run command ffprobe on file %s: %s", filePath, err)
	}

	var ratiosParams struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		}
	}

	err = json.Unmarshal(cmdBuffer.Bytes(), &ratiosParams)
	if err != nil {
		return "", fmt.Errorf("failed to unmarshal params: %s", err)
	}

	if len(ratiosParams.Streams) == 0 {
		return "", errors.New("no video streams found")
	}

	width := ratiosParams.Streams[0].Width
	height := ratiosParams.Streams[0].Height

	return classifyAspectRatio(width, height), nil
}

func classifyAspectRatio(width, height int) string {
	if width == 16*height/9 {
		return "16:9"
	} else if height == 16*width/9 {
		return "9:16"
	}
	return "other"
}

func processVideoForFastStart(filePath string) (string, error) {
	processedFilePath := fmt.Sprintf("%s.processing", filePath)
	cmd := exec.Command("ffmpeg",
		"-i", filePath,
		"-c", "copy",
		"-movflags", "faststart",
		"-f", "mp4",
		processedFilePath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("error processing video: %s, %v", stderr.String(), err)
	}

	fileInfo, err := os.Stat(processedFilePath)
	if err != nil {
		return "", fmt.Errorf("could not stat processed file: %v", err)
	}
	if fileInfo.Size() == 0 {
		return "", fmt.Errorf("processed file is empty")
	}
	return processedFilePath, nil
}
