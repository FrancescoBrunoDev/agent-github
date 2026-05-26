#!/usr/bin/env bash
set -euo pipefail

echo "==> Agent worker starting for ${REPO_URL}#${ISSUE_NUMBER}"
if [ "${AGENT_CONTINUE:-false}" = "true" ]; then
    echo "==> Mode: CONTINUATION"
else
    echo "==> Mode: NEW"
fi
echo "==> Platform: ${AGENT_PLATFORM:-github}"

# Detect platform
PLATFORM="${AGENT_PLATFORM:-github}"

# Parse repo owner/name from URL (works for both GitHub and Gitea)
REPO_OWNER=$(echo "${REPO_URL}" | sed -n 's|https://[^/]*/\([^/]*\)/.*|\1|p')
REPO_NAME=$(echo "${REPO_URL}" | sed -n 's|https://[^/]*/[^/]*/\([^.]*\)\(.git\)\?$|\1|p')
FULL_REPO="${REPO_OWNER}/${REPO_NAME}"

# ---- Set up pi agent configuration ----
mkdir -p ~/.pi/agent

if [ -n "${AGENT_PI_SETTINGS:-}" ] && [ "${AGENT_PI_SETTINGS}" != "{}" ]; then
    echo "${AGENT_PI_SETTINGS}" > ~/.pi/agent/settings.json
    echo "==> Pi settings written"
fi

if [ -n "${AGENT_PI_MODELS:-}" ] && [ "${AGENT_PI_MODELS}" != "{}" ]; then
    echo "${AGENT_PI_MODELS}" > ~/.pi/agent/models.json
    echo "==> Pi models written"
fi

if [ -n "${AGENT_PI_AUTH:-}" ] && [ "${AGENT_PI_AUTH}" != "{}" ]; then
    echo "${AGENT_PI_AUTH}" > ~/.pi/agent/auth.json
    echo "==> Pi auth written"
fi

# Auth: use bot token for CLI tools (comments, PRs) if available, else installation/PAT token
if [ "$PLATFORM" = "gitea" ]; then
    # Gitea: use tea CLI with Gitea token
    if [ -n "${AGENT_GITEA_BOT_TOKEN:-}" ]; then
        GITEA_TOKEN="${AGENT_GITEA_BOT_TOKEN}"
    else
        GITEA_TOKEN="${GITHUB_TOKEN}"
    fi

    GITEA_BASE="${AGENT_GITEA_BASE_URL:-https://gitea.com}"
    echo "==> Logging into Gitea at ${GITEA_BASE}..."
    tea login add --name agent --url "${GITEA_BASE}" --token "${GITEA_TOKEN}" 2>&1 || true
    tea login default agent 2>&1 || true
    tea logins 2>&1 || true
else
    # GitHub: use gh CLI
    if [ -n "${AGENT_BOT_TOKEN:-}" ]; then
        export GH_TOKEN="${AGENT_BOT_TOKEN}"
        echo "==> Using bot user token for gh operations"
    else
        export GH_TOKEN="${GITHUB_TOKEN}"
    fi
    gh auth status 2>&1 || true
fi

# ---- Clone repo (always uses installation/PAT token via URL) ----
if [ -z "${GITHUB_TOKEN:-}" ]; then
    echo "==> ERROR: GITHUB_TOKEN is empty"
    exit 1
fi

# Adapt auth URL format for platform
if [ "$PLATFORM" = "gitea" ]; then
    AUTH_REPO_URL=$(echo "${REPO_URL}" | sed "s|https://|https://oauth2:${GITHUB_TOKEN}@|")
else
    AUTH_REPO_URL=$(echo "${REPO_URL}" | sed "s|https://|https://x-access-token:${GITHUB_TOKEN}@|")
fi
git clone "${AUTH_REPO_URL}" /workspace 2>&1
cd /workspace

git config user.name "${AGENT_BOT_USER:-Agent}"
git config user.email "${AGENT_BOT_USER:-agent}@francesco-bruno.com"
git config remote.origin.url "${AUTH_REPO_URL}"

# ---- Helper functions that abstract platform differences ----

post_comment() {
    local issue_num="$1"
    local body="$2"
    if [ "$PLATFORM" = "gitea" ]; then
        tea comment --repo "${REPO_OWNER}/${REPO_NAME}" "${issue_num}" "${body}" 2>&1 || true
    else
        gh issue comment "${issue_num}" --body "${body}" 2>&1 || true
    fi
}

delete_comment() {
    local comment_id="$1"
    if [ "$PLATFORM" = "gitea" ]; then
        curl -s -X DELETE "${GITEA_BASE}/api/v1/repos/${REPO_OWNER}/${REPO_NAME}/issues/comments/${comment_id}" \
            -H "Authorization: token ${GITEA_TOKEN}" 2>&1 || true
    else
        gh api "repos/${REPO_OWNER}/${REPO_NAME}/issues/comments/${comment_id}" -X DELETE 2>&1 || true
    fi
}

create_pr() {
    local title="$1"
    local body="$2"
    if [ "$PLATFORM" = "gitea" ]; then
        tea pr create --repo "${REPO_OWNER}/${REPO_NAME}" \
            --head "${BRANCH}" --base main \
            --title "${title}" --description "${body}" 2>&1 || true
    else
        gh pr create --title "${title}" --body "${body}" 2>&1 || true
    fi
}

get_pr_number() {
    if [ "$PLATFORM" = "gitea" ]; then
        # tea pr list doesn't support --head; filter with jq
        tea pr list --repo "${REPO_OWNER}/${REPO_NAME}" --state open --output json 2>/dev/null | jq -r ".[] | select(.head.label == \"${BRANCH}\") | .number" 2>/dev/null | head -1 || echo ""
    else
        gh pr list --head "$BRANCH" --json number --jq '.[0].number' 2>/dev/null || echo ""
    fi
}

pr_comment() {
    local pr_num="$1"
    local body="$2"
    if [ "$PLATFORM" = "gitea" ]; then
        tea comment --repo "${REPO_OWNER}/${REPO_NAME}" "${pr_num}" "${body}" 2>&1 || true
    else
        gh pr comment "${pr_num}" --body "${body}" 2>&1 || true
    fi
}

# ---- Determine branch ----
BRANCH="${AGENT_BRANCH:-agent/issue-${ISSUE_NUMBER}}"

# ---- Fetch conversation ----
CONVERSATION="## Original issue\n\n**Title:** ${ISSUE_TITLE}\n\n${ISSUE_BODY}"
if [ "$PLATFORM" = "gitea" ]; then
    COMMENTS_JSON=$(tea comments --repo "${REPO_OWNER}/${REPO_NAME}" --output json 2>/dev/null | jq -r '.[] | "**@" + .poster.login + ":** " + .body' 2>/dev/null || echo "")
else
    COMMENTS_JSON=$(gh api "repos/${REPO_OWNER}/${REPO_NAME}/issues/${ISSUE_NUMBER}/comments" --jq '.[] | "**@" + .user.login + ":** " + .body' 2>/dev/null || echo "")
fi
if [ -n "${COMMENTS_JSON}" ]; then
    CONVERSATION="${CONVERSATION}\n\n## Comments\n\n${COMMENTS_JSON}"
fi

# ---- Post "working" comment (save ID for later deletion) ----
WORKING_MSG="🔧 I'm working on this..."
if [ "$PLATFORM" = "gitea" ]; then
    WORKING_COMMENT_URL=$(tea comment --repo "${REPO_OWNER}/${REPO_NAME}" "${ISSUE_NUMBER}" "${WORKING_MSG}" 2>&1 || true)
else
    WORKING_COMMENT_URL=$(gh issue comment "${ISSUE_NUMBER}" --body "${WORKING_MSG}" 2>&1 || true)
fi
echo "Working comment: ${WORKING_COMMENT_URL}"
WORKING_COMMENT_ID=$(echo "${WORKING_COMMENT_URL}" | grep -oP 'issuecomment-\K[0-9]+' || echo "")

# ---- Self-assign issue (if bot user configured and not already assigned) ----
# The classifier already handles assignment; this is a fallback for mention-triggered jobs.
if [ "${AGENT_TRIGGER_TYPE:-}" != "assign" ] && [ -n "${AGENT_BOT_USER:-}" ] && [ -n "${AGENT_BOT_TOKEN:-}" ]; then
    echo "==> Self-assigning issue to ${AGENT_BOT_USER}..."
    if [ "$PLATFORM" = "gitea" ]; then
        curl -s -X PATCH "${GITEA_BASE}/api/v1/repos/${REPO_OWNER}/${REPO_NAME}/issues/${ISSUE_NUMBER}" \
            -H "Authorization: token ${GITEA_TOKEN}" \
            -H "Content-Type: application/json" \
            -d "{\"assignees\":[\"${AGENT_BOT_USER}\"]}" 2>&1 || true
    else
        gh issue edit "${ISSUE_NUMBER}" --add-assignee "${AGENT_BOT_USER}" 2>&1 || true
    fi
fi

# ---- Branch / prompt logic ----
if [ "${AGENT_CONTINUE:-false}" = "true" ]; then
    echo "==> CONTINUATION mode — checking out ${BRANCH}"
    git fetch origin "${BRANCH}" 2>&1 || {
        BRANCH_MSG="❌ I couldn't find the branch \`${BRANCH}\` to continue."
        if [ "$PLATFORM" = "gitea" ]; then
            tea comment --repo "${REPO_OWNER}/${REPO_NAME}" "${ISSUE_NUMBER}" "${BRANCH_MSG}" 2>&1 || true
        else
            gh issue comment "${ISSUE_NUMBER}" --body "${BRANCH_MSG}" 2>&1 || true
        fi
        exit 1
    }
    git checkout "${BRANCH}" 2>&1

    cat > /tmp/prompt.md <<PROMPT
You are Agent, an AI agent that solves issues on ${PLATFORM} forges.

This is a **continuation** of your previous work on issue #${ISSUE_NUMBER}.
A reviewer left feedback on your pull request. Read the full conversation.

## Full Conversation

${CONVERSATION}

## Latest Feedback

${AGENT_FEEDBACK}

## Instructions

0. **First**: if AGENTS.md doesn't exist at the repo root, create one (see new-issue instructions for format). Commit it separately.
1. Read the feedback and full conversation carefully.
2. If you need clarification, use \`$(if [ "$PLATFORM" = "gitea" ]; then echo "tea comment --repo ${REPO_OWNER}/${REPO_NAME} ${ISSUE_NUMBER}"; else echo "gh issue comment ${ISSUE_NUMBER} --body"; fi)" ...\` to ask.
3. Implement the requested changes. Use \`agent-browser\` to test UI changes if applicable.
4. When done: commit with a descriptive message, push to \`${BRANCH}\`.
5. If you took screenshots with agent-browser, save them in /workspace/.
6. Write a summary comment:
   \`$(if [ "$PLATFORM" = "gitea" ]; then echo "tea comment --repo ${REPO_OWNER}/${REPO_NAME} ${ISSUE_NUMBER} \"...\""; else echo "gh issue comment ${ISSUE_NUMBER} --body"; fi) "..."\`
PROMPT
else
    echo "==> NEW issue mode — will create branch ${BRANCH}"

    cat > /tmp/prompt.md <<PROMPT
You are Agent, an AI agent that solves issues on ${PLATFORM} forges.

Your task is to resolve the following issue.

## Full Conversation

${CONVERSATION}

## Instructions

0. **First**: check if an AGENTS.md or AGENT.md file exists at the repo root. If not, create AGENTS.md documenting:
   - Project overview and purpose
   - Tech stack (languages, frameworks, build tools)
   - Directory structure
   - How to build, run, and test
   - Key conventions and dependencies
   Read package.json, config files, and source layout to gather this info. Keep it concise.
   Commit this file before starting the fix: \`git add AGENTS.md && git commit -m "docs: add AGENTS.md"\`
1. Understand the issue and all comments thoroughly.
2. If you need clarification, use \`$(if [ "$PLATFORM" = "gitea" ]; then echo "tea comment --repo ${REPO_OWNER}/${REPO_NAME} ${ISSUE_NUMBER} \"...\""; else echo "gh issue comment ${ISSUE_NUMBER} --body"; fi) "..."\` to ask.
3. Implement the fix. Use \`agent-browser\` to test UI changes if applicable.
4. If your fix produces static/build output (e.g., from npm run build), copy it to a preview/ directory in /workspace/.
5. When done:
   - \`git checkout -b ${BRANCH}\`
   - \`git commit -am "fix: ${ISSUE_TITLE}"\`
   - \`git push origin ${BRANCH}\`
   - $(if [ "$PLATFORM" = "gitea" ]; then echo "\`tea pr create --repo ${REPO_OWNER}/${REPO_NAME} --head ${BRANCH} --base main --title \"Fix #${ISSUE_NUMBER}: ${ISSUE_TITLE}\" --description \"...\"\`"; else echo "\`gh pr create --title \"Fix #${ISSUE_NUMBER}: ${ISSUE_TITLE}\" --body \"...\"\`"; fi)
6. If you took screenshots with agent-browser, save them in /workspace/.
7. Write a summary comment:
   \`$(if [ "$PLATFORM" = "gitea" ]; then echo "tea comment --repo ${REPO_OWNER}/${REPO_NAME} ${ISSUE_NUMBER} \"...\""; else echo "gh issue comment ${ISSUE_NUMBER} --body"; fi) "..."\`
PROMPT
fi

# ---- Set up devcontainer ----
if [ -f .devcontainer/devcontainer.json ]; then
    echo "==> Devcontainer found, setting up environment..."
    eval "$(~/.local/bin/mise activate bash)" 2>/dev/null || true

    NODE_VERSION=$(grep -oP '"version":\s*"\K[^"]+' .devcontainer/devcontainer.json 2>/dev/null | head -1 || echo "")
    if [ -z "${NODE_VERSION}" ]; then
        NODE_VERSION=$(grep -oP 'typescript-node:\K[0-9]+' .devcontainer/devcontainer.json 2>/dev/null || echo "")
    fi

    if [ -n "${NODE_VERSION}" ]; then
        echo "==> Installing Node.js ${NODE_VERSION} via mise..."
        mise install node@${NODE_VERSION} 2>&1 || true
        mise use node@${NODE_VERSION} 2>&1 || true
    else
        mise install node@lts 2>&1 || true
        mise use node@lts 2>&1 || true
    fi

    if [ -f package.json ]; then
        echo "==> Installing npm dependencies..."
        npm install 2>&1 || true
    fi

    if [ -f requirements.txt ]; then
        pip install -r requirements.txt 2>&1 || true
    fi

    echo "==> Environment ready (node $(node --version 2>/dev/null || echo '?'))"
fi

# ---- Snapshot existing workspace files (to only upload new screenshots) ----
find /workspace -maxdepth 1 -type f > /tmp/files-before-pi.txt 2>/dev/null || true

# ---- Invoke pi agent ----
echo "==> Invoking pi agent..."
pi --print @/tmp/prompt.md

# ---- Delete the "working" comment ----
if [ -n "${WORKING_COMMENT_ID:-}" ]; then
    echo "==> Removing working comment..."
    if [ "$PLATFORM" = "gitea" ]; then
        curl -s -X DELETE "${GITEA_BASE}/api/v1/repos/${REPO_OWNER}/${REPO_NAME}/issues/comments/${WORKING_COMMENT_ID}" \
            -H "Authorization: token ${GITEA_TOKEN}" 2>&1 || true
    else
        gh api "repos/${REPO_OWNER}/${REPO_NAME}/issues/comments/${WORKING_COMMENT_ID}" -X DELETE 2>&1 || true
    fi
fi

# ---- Upload screenshots to agent media server ----
echo "==> Checking for screenshots..."
mkdir -p /tmp/agent-upload /agent-media

# Only upload images created during this pi run (not stale repo files)
find /workspace -maxdepth 1 \( -name "*.png" -o -name "*.jpg" -o -name "*.gif" -o -name "*.webp" \) -type f 2>/dev/null | while read img; do
    if ! grep -qFx "$img" /tmp/files-before-pi.txt 2>/dev/null; then
        NEWNAME="issue-${ISSUE_NUMBER}-$(basename "${img}")"
        cp "${img}" "/tmp/agent-upload/${NEWNAME}"
        cp "${img}" "/agent-media/${NEWNAME}"
    fi
done

if [ "$(ls -A /tmp/agent-upload 2>/dev/null)" ]; then
    echo "==> Publishing screenshots..."
    
    COMMENT_BODY="📸 **Screenshots:**"
    for img in /tmp/agent-upload/*; do
        FNAME=$(basename "${img}")
        cp "${img}" "/agent-media/${FNAME}"
        COMMENT_BODY="${COMMENT_BODY}"$'\n'"![${FNAME}](${AGENT_BASE_URL}/media/${FNAME})"
    done
    post_comment "${ISSUE_NUMBER}" "${COMMENT_BODY}"
fi

rm -rf /tmp/agent-upload

# ---- Deploy containerized preview via Nixpacks ----
echo "==> Building containerized preview..."

# Clean up any previous preview container for this PR
PREVIEW_CONTAINER="agent-preview-${FULL_REPO}"
docker rm -f "${PREVIEW_CONTAINER}" 2>/dev/null || true

# Get PR URL and number (platform-aware)
if [ "$PLATFORM" = "gitea" ]; then
    PR_NUMBER=$(tea pr list --repo "${REPO_OWNER}/${REPO_NAME}" --state open --output json 2>/dev/null | jq -r ".[] | select(.head.label == \"${BRANCH}\") | .number" 2>/dev/null | head -1 || echo "")
    if [ -n "$PR_NUMBER" ]; then
        PR_URL="${GITEA_BASE}/${REPO_OWNER}/${REPO_NAME}/pulls/${PR_NUMBER}"
    fi
else
    PR_URL=$(gh pr list --head "$BRANCH" --json url --jq '.[0].url' 2>/dev/null || echo "")
    if [ -n "$PR_URL" ]; then
        PR_NUMBER=$(echo "$PR_URL" | grep -oP '/pull/\K[0-9]+' || echo "")
    fi
fi
if [ -n "$PR_NUMBER" ]; then
    if [ -n "$PR_NUMBER" ]; then
        PREVIEW_IMAGE="agent-preview-${FULL_REPO,,}-${PR_NUMBER}"
        PREVIEW_CONTAINER="${PREVIEW_IMAGE}"

        # Build image with Nixpacks (per-repo overrides supported)
        NIXPACKS_ARGS=()
        [ -n "${AGENT_NIXPACKS_INSTALL_CMD:-}" ] && NIXPACKS_ARGS+=(--install-cmd "${AGENT_NIXPACKS_INSTALL_CMD}")
        [ -n "${AGENT_NIXPACKS_BUILD_CMD:-}" ] && NIXPACKS_ARGS+=(--build-cmd "${AGENT_NIXPACKS_BUILD_CMD}")
        [ -n "${AGENT_NIXPACKS_START_CMD:-}" ] && NIXPACKS_ARGS+=(--start-cmd "${AGENT_NIXPACKS_START_CMD}")
        echo "==> Nixpacks building ${PREVIEW_IMAGE}..."
        if nixpacks build /workspace --name "${PREVIEW_IMAGE}" "${NIXPACKS_ARGS[@]}" 2>&1; then
            # Stop old container, run new one on random port
            docker rm -f "${PREVIEW_CONTAINER}" 2>/dev/null || true
            docker run -d --name "${PREVIEW_CONTAINER}" \
                -p 0:3000 -p 0:8080 \
                --restart unless-stopped \
                "${PREVIEW_IMAGE}" 2>&1
            
            # Verify container is actually running
            if [ "$(docker inspect -f '{{.State.Running}}' "${PREVIEW_CONTAINER}" 2>/dev/null)" = "true" ]; then
                # Wait for the server to bind (npx may download serve first)
                PORT=""
                for i in $(seq 1 10); do
                    PORT=$(docker port "${PREVIEW_CONTAINER}" 3000 2>/dev/null | head -1 | cut -d: -f2 || echo "")
                    if [ -z "$PORT" ]; then
                        PORT=$(docker port "${PREVIEW_CONTAINER}" 8080 2>/dev/null | head -1 | cut -d: -f2 || echo "")
                    fi
                    [ -n "$PORT" ] && break
                    sleep 1
                done
                
                if [ -n "$PORT" ]; then
                    mkdir -p /agent-preview
                    echo "$PORT" > "/agent-preview/${FULL_REPO}-${PR_NUMBER}.port"
                    echo "==> Preview running on port ${PORT}"
                    post_comment "${ISSUE_NUMBER}" "🌐 **Preview:** ${AGENT_BASE_URL}/preview/${PLATFORM}/${REPO_OWNER}/${REPO_NAME}/${PR_NUMBER}/" 2>&1 || true
                else
                    echo "==> WARNING: could not determine preview port"
                    docker rm -f "${PREVIEW_CONTAINER}" 2>/dev/null || true
                fi
            else
                echo "==> Container exited immediately (no start command), cleaning up..."
                docker rm -f "${PREVIEW_CONTAINER}" 2>/dev/null || true
            fi
        else
            echo "==> Nixpacks build failed (no start command), posting notice..."
            PREVIEW_BUILT=false

            # Retry 1: nginx serving build output
            if [ "$PREVIEW_BUILT" != true ]; then
                echo "==> Trying static nginx preview..."
                STATIC_DIR=""
                for dir in dist build out public; do
                    if [ -d "/workspace/$dir" ] && [ "$(ls -A /workspace/$dir 2>/dev/null)" ]; then
                        STATIC_DIR="$dir"
                        break
                    fi
                done

                if [ -n "$STATIC_DIR" ]; then
                    echo "==> Found build output: /workspace/$STATIC_DIR"
                    cat > /workspace/Dockerfile.agent <<DOCKERFILE
FROM nginx:alpine
COPY $STATIC_DIR /usr/share/nginx/html
DOCKERFILE
                    echo "==> Building static preview from /workspace/$STATIC_DIR..."
                    BUILD_OUT=$(docker build -t "${PREVIEW_IMAGE}" -f /workspace/Dockerfile.agent /workspace 2>&1)
                    BUILD_EXIT=$?
                    echo "$BUILD_OUT"
                    if [ $BUILD_EXIT -eq 0 ]; then
                        docker rm -f "${PREVIEW_CONTAINER}" 2>/dev/null || true
                        docker run -d --name "${PREVIEW_CONTAINER}" -p 0:80 "${PREVIEW_IMAGE}" 2>&1
                        PORT=""
                        for i in $(seq 1 5); do
                            PORT=$(docker port "${PREVIEW_CONTAINER}" 80 2>/dev/null | head -1 | cut -d: -f2 || echo "")
                            [ -n "$PORT" ] && break
                            sleep 1
                        done
                        if [ -n "$PORT" ]; then
                            mkdir -p /agent-preview
                            echo "$PORT" > "/agent-preview/${FULL_REPO}-${PR_NUMBER}.port"
                            echo "==> Static preview running on port ${PORT}"
                            post_comment "${ISSUE_NUMBER}" "🌐 **Preview:** ${AGENT_BASE_URL}/preview/${PLATFORM}/${REPO_OWNER}/${REPO_NAME}/${PR_NUMBER}/" 2>&1 || true
                            PREVIEW_BUILT=true
                        else
                            docker rm -f "${PREVIEW_CONTAINER}" 2>/dev/null || true
                        fi
                    fi
                else
                    echo "==> No build output directory found (tried: dist build out public)"
                fi
            fi

            # Retry 2: raw file copy
            if [ "$PREVIEW_BUILT" != true ]; then
                echo "==> Falling back to file copy..."
                if [ -d /workspace/preview ] && [ "$(ls -A /workspace/preview 2>/dev/null)" ]; then
                    mkdir -p "/agent-preview/${FULL_REPO}/${PR_NUMBER}"
                    cp -r /workspace/preview/* "/agent-preview/${FULL_REPO}/${PR_NUMBER}/" 2>&1
                    post_comment "${ISSUE_NUMBER}" "🌐 **Preview:** ${AGENT_BASE_URL}/preview/${PLATFORM}/${REPO_OWNER}/${REPO_NAME}/${PR_NUMBER}/" 2>&1 || true
                    PREVIEW_BUILT=true
                fi
            fi

            # All retries failed
            if [ "$PREVIEW_BUILT" != true ]; then
                post_comment "${ISSUE_NUMBER}" "❌ No preview available — add a \`start\` script to package.json (e.g. \`\"start\": \"npx serve dist\"\`) to enable containerized previews." 2>&1 || true
            fi

            # Comment preview link on the PR
            PREVIEW_LINK="${AGENT_BASE_URL}/preview/${PLATFORM}/${REPO_OWNER}/${REPO_NAME}/${PR_NUMBER}/"
            if [ "$PREVIEW_BUILT" = true ]; then
                pr_comment "${PR_NUMBER}" "🌐 **Preview:** ${PREVIEW_LINK}" 2>&1 || true
            fi
        fi
    fi
fi

echo "==> Worker done"
