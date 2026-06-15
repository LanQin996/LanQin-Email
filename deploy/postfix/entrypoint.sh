#!/bin/sh
set -eu
: "${LANQIN_PUBLIC_HOSTNAME:=mail.example.com}"
postconf -e "myhostname = ${LANQIN_PUBLIC_HOSTNAME}"
postconf -e "myorigin = ${LANQIN_PUBLIC_HOSTNAME}"
postconf -e "milter_mail_macros = i {mail_addr} {client_addr} {client_name} {auth_authen}"
postconf -e "smtpd_milters = inet:rspamd:11332"
postconf -e "non_smtpd_milters = inet:rspamd:11332"
postfix check
exec postfix start-fg
