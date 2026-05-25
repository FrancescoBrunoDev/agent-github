#!/usr/bin/env bash
set -euo pipefail

echo "==> Otto worker starting for ${REPO_URL}#${ISSUE_NUMBER}"
echo "==> Mode: ${OTTO_CONTINUE:+CONTINUATION}${OTTO_CONTINUE:-NEW}"

REPO_OWNER=$(echo "${REPO_URL}" | sed -n 's|https://github.com/\([^/]*\)/.*|\1|p')
REPO_NAME=$(echo "${REPO_URL}" | sed -n 's|https://github.com/[^/]*/\([^.]*\).*|\1|p')

# ---- Set up pi agent configuration ----
mkdir -p ~/.pi/agent

if [ -n "${OTTO_PI_SETTINGS:-}" ] && [ "${OTTO_PI_SETTINGS}" != "{}" ]; then
    echo "${OTTO_PI_SETTINGS}" > ~/.pi/agent/settings.json
    echo "==> Pi settings written"
fi

if [ -n "${OTTO_PI_MODELS:-}" ] && [ "${OTTO_PI_MODELS}" != "{}" ]; then
    echo "${OTTO_PI_MODELS}" > ~/.pi/agent/models.json
    echo "==> Pi models written"
fi

# Auth: use bot token for gh CLI (comments, PRs) if available, else installation token
if [ -n "${OTTO_BOT_TOKEN:-}" ]; then
    export GH_TOKEN="${OTTO_BOT_TOKEN}"
    echo "==> Using bot user token for gh operations"
else
    export GH_TOKEN="${GITHUB_TOKEN}"
fi
gh auth status 2>&1 || true

# ---- Clone repo (always uses installation token via URL) ----
if [ -z "${GITHUB_TOKEN:-}" ]; then
    echo "==> ERROR: GITHUB_TOKEN is empty"
    exit 1
fi
AUTH_REPO_URL=$(echo "${REPO_URL}" | sed "s|https://|https://x-access-token:${GITHUB_TOKEN}@|")
git clone "${AUTH_REPO_URL}" /workspace 2>&1
cd /workspace

git config user.name "${OTTO_BOT_USER:-Otto}"
git config user.email "${OTTO_BOT_USER:-otto}@francesco-bruno.com"
git config remote.origin.url "${AUTH_REPO_URL}"

# ---- Determine branch ----
BRANCH="${OTTO_BRANCH:-otto/issue-${ISSUE_NUMBER}}"

# ---- Fetch conversation ----
CONVERSATION="## Original issue\n\n**Title:** ${ISSUE_TITLE}\n\n${ISSUE_BODY}"
COMMENTS_JSON=$(gh api "repos/${REPO_OWNER}/${REPO_NAME}/issues/${ISSUE_NUMBER}/comments" --jq '.[] | "**@" + .user.login + ":** " + .body' 2>/dev/null || echo "")
if [ -n "${COMMENTS_JSON}" ]; then
    CONVERSATION="${CONVERSATION}\n\n## Comments\n\n${COMMENTS_JSON}"
fi

# ---- Post "working" comment (save ID for later deletion) ----
WORKING_COMMENT_URL=$(gh issue comment "${ISSUE_NUMBER}" --body "🔧 I'm working on this..." 2>&1 || true)
echo "Working comment: ${WORKING_COMMENT_URL}"
WORKING_COMMENT_ID=$(echo "${WORKING_COMMENT_URL}" | grep -oP 'issuecomment-\K[0-9]+' || echo "")

# ---- Self-assign issue (if bot user configured and not already assigned) ----
if [ "${OTTO_TRIGGER_TYPE:-}" != "assign" ] && [ -n "${OTTO_BOT_USER:-}" ] && [ -n "${OTTO_BOT_TOKEN:-}" ]; then
    echo "==> Self-assigning issue to ${OTTO_BOT_USER}..."
    gh issue edit "${ISSUE_NUMBER}" --add-assignee "${OTTO_BOT_USER}" 2>&1 || true
fi

# ---- Branch / prompt logic ----
if [ "${OTTO_CONTINUE:-false}" = "true" ]; then
    echo "==> CONTINUATION mode — checking out ${BRANCH}"
    git fetch origin "${BRANCH}" 2>&1 || {
        gh issue comment "${ISSUE_NUMBER}" --body "❌ I couldn't find the branch \`${BRANCH}\` to continue." 2>&1 || true
        exit 1
    }
    git checkout "${BRANCH}" 2>&1

    cat > /tmp/prompt.md <<PROMPT
You are Otto, an AI agent that solves GitHub issues.

This is a **continuation** of your previous work on issue #${ISSUE_NUMBER}.
A reviewer left feedback on your pull request. Read the full conversation.

## Full Conversation

${CONVERSATION}

## Latest Feedback

${OTTO_FEEDBACK}

## Instructions

0. **First**: if AGENTS.md doesn't exist at the repo root, create one (see new-issue instructions for format). Commit it separately.
1. Read the feedback and full conversation carefully.
2. If you need clarification, use \`gh issue comment ${ISSUE_NUMBER} --body "..."\` to ask.
3. Implement the requested changes. Use \`agent-browser\` to test UI changes if applicable.
4. When done: commit with a descriptive message, push to \`${BRANCH}\`. Update the PR body:
   \`gh pr edit --body "## Changes made
- ..."\`
6. If you took screenshots with agent-browser, save them in /workspace/.
7. Write a summary comment:
   \`gh issue comment ${ISSUE_NUMBER} --body "✅ Done! Here's what I changed:\n- ..."\`
PROMPT
else
    echo "==> NEW issue mode — will create branch ${BRANCH}"

    cat > /tmp/prompt.md <<PROMPT
You are Otto, an AI agent that solves GitHub issues.

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
2. If you need clarification, use \`gh issue comment ${ISSUE_NUMBER} --body "..."\` to ask.
3. Implement the fix. Use \`agent-browser\` to test UI changes if applicable.
4. If your fix produces static/build output (e.g., from npm run build), copy it to a preview/ directory in /workspace/.
5. When done:
   - \`git checkout -b ${BRANCH}\`
   - \`git commit -am "fix: ${ISSUE_TITLE}"\`
   - \`git push origin ${BRANCH}\`
   - \`gh pr create --title "Fix #${ISSUE_NUMBER}: ${ISSUE_TITLE}" --body "## What I changed
- ...

## Why
- ...

Closes #${ISSUE_NUMBER}"\`
6. If you took screenshots with agent-browser, save them in /workspace/.
7. Write a summary comment:
   \`gh issue comment ${ISSUE_NUMBER} --body "✅ Done! Here's what I did:\n- ..."\`
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
    gh api "repos/${REPO_OWNER}/${REPO_NAME}/issues/comments/${WORKING_COMMENT_ID}" -X DELETE 2>&1 || true
fi

# ---- Upload screenshots to Otto media server ----
echo "==> Checking for screenshots..."
mkdir -p /tmp/otto-upload /otto-media

# Only upload images created during this pi run (not stale repo files)
find /workspace -maxdepth 1 \( -name "*.png" -o -name "*.jpg" -o -name "*.gif" -o -name "*.webp" \) -type f 2>/dev/null | while read img; do
    if ! grep -qFx "$img" /tmp/files-before-pi.txt 2>/dev/null; then
        NEWNAME="issue-${ISSUE_NUMBER}-$(basename "${img}")"
        cp "${img}" "/tmp/otto-upload/${NEWNAME}"
        cp "${img}" "/otto-media/${NEWNAME}"
    fi
done

if [ "$(ls -A /tmp/otto-upload 2>/dev/null)" ]; then
    echo "==> Publishing screenshots via Otto..."
    
    COMMENT_BODY="📸 **Screenshots:**"
    for img in /tmp/otto-upload/*; do
        FNAME=$(basename "${img}")
        cp "${img}" "/otto-media/${FNAME}"
        COMMENT_BODY="${COMMENT_BODY}"$'\n'"![${FNAME}](${OTTO_BASE_URL}/media/${FNAME})"
    done
    gh issue comment "${ISSUE_NUMBER}" --body "${COMMENT_BODY}" 2>&1 || true
fi

rm -rf /tmp/otto-upload

# ---- Deploy containerized preview via Nixpacks ----
echo "==> Building containerized preview..."

# Clean up any previous preview container for this PR
PREVIEW_CONTAINER="otto-preview-${REPO_NAME}"
docker rm -f "${PREVIEW_CONTAINER}" 2>/dev/null || true

PR_URL=$(gh pr list --head "$BRANCH" --json url --jq '.[0].url' 2>/dev/null || echo "")
if [ -n "$PR_URL" ]; then
    PR_NUMBER=$(echo "$PR_URL" | grep -oP '/pull/\K[0-9]+' || echo "")
    if [ -n "$PR_NUMBER" ]; then
        PREVIEW_IMAGE="otto-preview-${REPO_NAME,,}-${PR_NUMBER}"
        PREVIEW_CONTAINER="${PREVIEW_IMAGE}"

        # Build image with Nixpacks
        echo "==> Nixpacks building ${PREVIEW_IMAGE}..."
        if nixpacks build /workspace --name "${PREVIEW_IMAGE}" 2>&1; then
            # Stop old container, run new one on random port
            docker rm -f "${PREVIEW_CONTAINER}" 2>/dev/null || true
            docker run -d --name "${PREVIEW_CONTAINER}" \
                -P \
                --restart unless-stopped \
                "${PREVIEW_IMAGE}" 2>&1
            
            # Verify container is actually running
            if [ "$(docker inspect -f '{{.State.Running}}' "${PREVIEW_CONTAINER}" 2>/dev/null)" = "true" ]; then
                # Get the assigned host port
                PORT=$(docker port "${PREVIEW_CONTAINER}" 3000 2>/dev/null | head -1 | cut -d: -f2 || echo "")
                if [ -z "$PORT" ]; then
                    PORT=$(docker port "${PREVIEW_CONTAINER}" 8080 2>/dev/null | head -1 | cut -d: -f2 || echo "")
                fi
                
                if [ -n "$PORT" ]; then
                    mkdir -p /otto-preview
                    echo "$PORT" > "/otto-preview/${REPO_NAME}-${PR_NUMBER}.port"
                    echo "==> Preview running on port ${PORT}"
                    gh issue comment "${ISSUE_NUMBER}" --body "🌐 **Preview:** ${OTTO_BASE_URL}/preview/${REPO_NAME}/${PR_NUMBER}/" 2>&1 || true
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
                    cat > /workspace/Dockerfile.otto <<DOCKERFILE
FROM nginx:alpine
COPY $STATIC_DIR /usr/share/nginx/html
DOCKERFILE
                    echo "==> Building static preview from /workspace/$STATIC_DIR..."
                    BUILD_OUT=$(docker build -t "${PREVIEW_IMAGE}" -f /workspace/Dockerfile.otto /workspace 2>&1)
                    BUILD_EXIT=$?
                    echo "$BUILD_OUT"
                    if [ $BUILD_EXIT -eq 0 ]; then
                        docker rm -f "${PREVIEW_CONTAINER}" 2>/dev/null || true
                        docker run -d --name "${PREVIEW_CONTAINER}" -P "${PREVIEW_IMAGE}" 2>&1
                        PORT=$(docker port "${PREVIEW_CONTAINER}" 80 2>/dev/null | head -1 | cut -d: -f2 || echo "")
                        if [ -n "$PORT" ]; then
                            mkdir -p /otto-preview
                            echo "$PORT" > "/otto-preview/${REPO_NAME}-${PR_NUMBER}.port"
                            echo "==> Static preview running on port ${PORT}"
                            gh issue comment "${ISSUE_NUMBER}" --body "🌐 **Preview:** ${OTTO_BASE_URL}/preview/${REPO_NAME}/${PR_NUMBER}/" 2>&1 || true
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
                    mkdir -p "/otto-preview/${REPO_NAME}/${PR_NUMBER}"
                    cp -r /workspace/preview/* "/otto-preview/${REPO_NAME}/${PR_NUMBER}/" 2>&1
                    gh issue comment "${ISSUE_NUMBER}" --body "🌐 **Preview:** ${OTTO_BASE_URL}/preview/${REPO_NAME}/${PR_NUMBER}/" 2>&1 || true
                    PREVIEW_BUILT=true
                fi
            fi

            # All retries failed
            if [ "$PREVIEW_BUILT" != true ]; then
                gh issue comment "${ISSUE_NUMBER}" --body "❌ No preview available — add a \`start\` script to package.json (e.g. \`\"start\": \"npx serve dist\"\`) to enable containerized previews." 2>&1 || true
            fi
        fi
    fi
fi

echo "==> Worker done"
