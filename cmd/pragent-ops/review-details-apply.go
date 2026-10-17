package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"

	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/review"
)

func writeReviewDetailsBackup(path string, snapshot reviewDetailsSnapshot) (returnErr error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, file.Close())
	}()
	if err := json.NewEncoder(file).Encode(snapshot); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var restored reviewDetailsSnapshot
	if err := json.Unmarshal(data, &restored); err != nil {
		return err
	}
	if !reflect.DeepEqual(restored, snapshot) {
		return errors.New("the review-details backup does not match the original snapshot")
	}
	return nil
}

func applyReviewDetails(ctx context.Context, client *githubapp.Client, snapshot reviewDetailsSnapshot, botLogin string) error {
	if err := verifyReviewDetails(ctx, client, snapshot, snapshot.Comment.Body); err != nil {
		return err
	}
	updated, err := client.UpdateIssueComment(ctx, snapshot.Original.InstallationID, snapshot.Original.Repository, snapshot.Comment.ID, snapshot.Repaired)
	if err != nil {
		return err
	}
	if updated.ID != snapshot.Comment.ID || updated.Author != snapshot.Comment.Author || updated.Body != snapshot.Repaired {
		return errors.New("the summary update did not return the expected comment")
	}
	if err := verifyReviewDetails(ctx, client, snapshot, snapshot.Repaired); err != nil {
		return err
	}
	repeated, err := review.RestoreReviewDetails(snapshot.Repaired, snapshot.Source.Summary, snapshot.Original.Threads, botLogin)
	if err != nil {
		return err
	}
	if repeated != snapshot.Repaired {
		return errors.New("the restored review details did not pass the idempotence check")
	}
	return nil
}

func verifyReviewDetails(ctx context.Context, client *githubapp.Client, snapshot reviewDetailsSnapshot, expectedBody string) error {
	original := snapshot.Original
	current, err := captureReviewBodies(ctx, client, original.InstallationID, original.Repository, original.Number)
	if err != nil {
		return err
	}
	source, err := client.GetCheckRunDetails(ctx, original.InstallationID, original.Repository, snapshot.Source.Run.ID)
	if err != nil {
		return err
	}
	return compareReviewDetails(snapshot, expectedBody, current, source)
}

func compareReviewDetails(snapshot reviewDetailsSnapshot, expectedBody string, current reviewBodySnapshot, source githubapp.CheckRunDetails) error {
	original := snapshot.Original
	if current.PullRequest != original.PullRequest {
		return errors.New("pull request metadata changed during review-details repair")
	}
	if !reflect.DeepEqual(current.Reviews, original.Reviews) {
		return errors.New("native reviews changed during review-details repair")
	}
	if !reflect.DeepEqual(current.Threads, original.Threads) {
		return errors.New("inline comments changed during review-details repair")
	}
	if len(current.Comments) != len(original.Comments) {
		return errors.New("the summary comment count changed during review-details repair")
	}
	for index, expected := range original.Comments {
		if expected.ID == snapshot.Comment.ID {
			expected.Body = expectedBody
		}
		if current.Comments[index] != expected {
			return errors.New("a summary comment no longer matches the expected snapshot")
		}
	}
	if !reflect.DeepEqual(source, snapshot.Source) {
		return errors.New("the source check changed during review-details repair")
	}
	return nil
}
