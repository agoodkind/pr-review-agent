package review

func metadataRecordFromSummary(summary Summary) metadataReviewRecord {
	return metadataReviewRecord{
		Head: summary.Head, Revision: summary.MetadataRevision, Findings: summary.Metadata,
		Omissions: metadataOmissions(summary.Omissions), OmissionsAccepted: summary.OmissionsAccepted, DecisionReason: summary.DecisionReason,
	}
}

func metadataOmissions(omissions []unreadHunk) []unreadHunk {
	var metadata []unreadHunk
	for _, omission := range omissions {
		if omission.Target != nil {
			metadata = append(metadata, omission)
		}
	}
	return metadata
}

func updateMetadataOmissions(previous []unreadHunk, accepted bool, reason string, metadata metadataReviewRecord) ([]unreadHunk, bool, string) {
	var omissions []unreadHunk
	for _, omission := range previous {
		if omission.Target == nil {
			omissions = append(omissions, omission)
		}
	}
	if len(omissions) == 0 {
		accepted = true
		reason = ""
	}
	if len(metadata.Omissions) > 0 && !metadata.OmissionsAccepted {
		accepted = false
		reason = metadata.DecisionReason
	}
	omissions = append(omissions, metadata.Omissions...)
	return omissions, accepted, reason
}
