FROM debian:11.2 AS first
RUN apt update
RUN apt install -y apt-transport-https curl python3 vim
RUN curl -Lo minikube https://storage.googleapis.com/minikube/releases/latest/minikube-linux-amd64
RUN chmod +x minikube
RUN mv minikube /usr/local/bin
RUN echo "first stage complete"

FROM python:3.10.1
RUN apt-get update && \
    apt-get install -y python3-opencv && \
    rm -rf /var/lib/apt/lists/* \
RUN pip install pipenv waitress
COPY --from=first /usr/local/bin/minikube /usr/local/bin/minikube
