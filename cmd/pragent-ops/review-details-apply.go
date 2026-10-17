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
		return errors.New("the new private review-details backup could not be created")
	}
	defer func() {
		returnErr = errors.Join(returnErr, file.Close())
	}()
	if err := file.Chmod(0o600); err != nil {
		return errors.New("the review-details backup permissions could not be set")
	}
	if err := json.NewEncoder(file).Encode(snapshot); err != nil {
		return errors.New("the original review details could not be backed up")
	}
	if err := file.Sync(); err != nil {
		return errors.New("the review-details backup could not be synchronized")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return errors.New("the review-details backup could not be read for verification")
	}
	var restored reviewDetailsSnapshot
	if json.Unmarshal(data, &restored) != nil || !reflect.DeepEqual(restored, snapshot) {
		return errors.New("the review-details backup does not match the original snapshot")
	}
	return nil
}

func applyReviewDetails(ctx context.Context, client *githubapp.Client, snapshot reviewDetailsSnapshot, botLogin string) error {
	if err := verifyReviewDetails(ctx, client, snapshot, snapshot.Comment.Body); err != nil {
		return err
	}
	updated, updateErr := client.UpdateIssueComment(ctx, snapshot.Original.InstallationID, snapshot.Original.Repository, snapshot.Comment.ID, snapshot.Repaired)
	if updateErr != nil {
		// GitHub may accept an update before the response fails.
		if err := verifyReviewDetails(ctx, client, snapshot, snapshot.Repaired); err != nil {
			return errors.New("the summary update or its verification failed; retain the private backup")
		}
	} else if updated.ID != snapshot.Comment.ID || updated.Author != snapshot.Comment.Author || updated.Body != snapshot.Repaired {
		return errors.New("the summary update did not return the expected comment")
	}
	if err := verifyReviewDetails(ctx, client, snapshot, snapshot.Repaired); err != nil {
		return err
	}
	repeated, err := review.RestoreReviewDetails(snapshot.Repaired, snapshot.Source.Summary, snapshot.Original.Threads, botLogin)
	if err != nil || repeated != snapshot.Repaired {
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
	source, err := client.GetCheckRunDetails(ctx, original.InstallationID, original.Repository, snapshot.Source.Run.ID)
	if err != nil {
		return errors.New("the source check could not be read for verification")
	}
	if !reflect.DeepEqual(source, snapshot.Source) {
		return errors.New("the source check changed during review-details repair")
	}
	return nil
}
