package main

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"

	"github.com/OferRavid/learn-file-storage-s3-golang-starter/internal/auth"
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

	tmpVidFile, err := os.CreateTemp(cfg.assetsRoot, "tubely_upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to create file on server", err)
		return
	}
	defer os.Remove(tmpVidFile.Name())
	defer tmpVidFile.Close()

	_, err = io.Copy(tmpVidFile, videoFile)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to copy data to file", err)
	}

	tmpVidFile.Seek(0, io.SeekStart)

	assetPath := getAssetPath(mediaType)
	videoURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, assetPath)
	video.VideoURL = &videoURL

	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &assetPath,
		Body:        tmpVidFile,
		ContentType: &mediaType,
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
