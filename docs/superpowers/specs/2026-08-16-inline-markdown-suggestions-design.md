# Inline Markdown and Suggestions

Inline findings will format code references as inline code and will offer GitHub's commit action only for exact replacements.

## Output contract

Each finding uses one short Markdown heading and one concise paragraph. The paragraph states the defect, impact, and fix. Every code symbol, expression, environment variable, function name, type name, and literal uses inline code formatting.

The model returns the replacement separately from the explanatory prose. An empty replacement means the finding has no safe commit suggestion.

The model may return a replacement only when it completely replaces the finding's anchored changed line range. The replacement must be syntactically complete for that range and must preserve required indentation. A fix that needs omitted context, another file, or another line range leaves the replacement empty.

## Data contract

Each structured finding adds a required `suggestion` string. Strict model output requires the field for every finding. Existing findings use an empty string when no replacement qualifies.

Finding validation rejects suggestion text that contains a fenced Markdown delimiter. This prevents replacement text from ending the suggestion block early.

The stable finding identity remains based on the path and heading. Adding or changing a replacement does not create a new historical identity.

## Rendering

The renderer preserves the heading and paragraph Markdown. When `suggestion` is nonempty, it appends a fenced `suggestion` block after the paragraph. GitHub then displays the commit suggestion action on the inline review comment.

The hidden finding marker remains last. Marker decoding excludes the suggestion block from the explanatory body and reads historical findings created before this field existed.

## Prompt policy

The review prompt requires inline code formatting for every code reference. It also requires an empty suggestion unless the model can provide the exact replacement for the anchored range.

The prompt forbids explanatory text inside `suggestion`. The replacement contains source code only.

## Verification

Public behavior tests submit a realistic review through the GitHub client boundary. They verify the rendered inline body, line range, hidden marker, and native `suggestion` fence.

Schema tests prove the model contract requires `suggestion`. Validation tests prove empty suggestions remain valid and malformed fenced suggestions fail.

The temporary pull request provides live proof. One eligible finding must render code references with backticks and expose GitHub's commit suggestion action. The temporary pull request never merges.
