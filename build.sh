#cd data
#go-bindata -nocompress  -nomemcopy  -o spem.go server/
#go-bindata -nocompress  -nomemcopy  -o cpem.go client/
#mv spem.go ../src/server
#mv cpem.go ../src/client
#cd ..

#-A INPUT -p tcp -m state --state NEW -m tcp --dport 443 -j ACCEPT
#-A FORWARD -j RH-Firewall-1-INPUT
#-A RH-Firewall-1-INPUT -i lo -j ACCEPT
#-A RH-Firewall-1-INPUT -p icmp -m icmp --icmp-type any -j ACCEPT
#-A RH-Firewall-1-INPUT -p esp -j ACCEPT
#-A RH-Firewall-1-INPUT -p ah -j ACCEPT
#-A RH-Firewall-1-INPUT -d 224.0.0.251 -p udp -m udp --dport 5353 -j ACCEPT
#-A RH-Firewall-1-INPUT -p udp -m udp --dport 631 -j ACCEPT
#-A RH-Firewall-1-INPUT -p tcp -m tcp --dport 631 -j ACCEPT
#-A RH-Firewall-1-INPUT -m state --state RELATED,ESTABLISHED -j ACCEPT

#iptables -I INPUT 5 -p tcp -m state --state NEW -m tcp --dport 18887 -j ACCEPT
#iptables -I INPUT 6 -p udp -m udp --dport 18887 -j ACCEPT
#iptables -I INPUT 7 -p tcp -m tcp --dport 18887 -j ACCEPT

#GOARCH=arm GOARM=5 
go build -o stest src/main_server.go
go build -o ctest src/main_client.go
#GOARCH=arm GOARM=5 go build -o atest src/main_assist.go
go build -o atest src/main_assist.go
cp stest ctest atest /home/janson/workspace-go/src/github.com/jannson/routercenter/statics
