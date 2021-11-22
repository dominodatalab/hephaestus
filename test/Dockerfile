FROM debian:11.1

RUN apt update
RUN apt install -y apt-transport-https curl python3 vim
RUN curl -Lo minikube https://storage.googleapis.com/minikube/releases/latest/minikube-linux-amd64
RUN chmod +x minikube
RUN mv minikube /usr/local/bin
