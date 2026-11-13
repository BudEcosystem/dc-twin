-- NVCM 1.4.1's databases and owners (the chart's CloudNativePG clusters, the installer's names)
CREATE ROLE nautobot LOGIN PASSWORD 'nvcm-db';
CREATE DATABASE nautobot OWNER nautobot;
CREATE ROLE config_store LOGIN PASSWORD 'nvcm-db';
CREATE DATABASE config_store OWNER config_store;
CREATE ROLE dhcp LOGIN PASSWORD 'nvcm-db';
CREATE DATABASE kea_dhcp OWNER dhcp;
