# kubernetes-internal-cd

Needed environment variables:

- SLACK_URL: The slack webhook url to post messages to a slack channel
- PORT: The port to run on. Defaults to 8080
- SECRET_NAMESPACE: The namespace where the secret is located for the hmac master key
- SECRET_NAME: The name of the secret containing the hmac master key
